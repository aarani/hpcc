package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/daemon/client"
	"github.com/aarani/hpcc/internal/daemon/dispatch"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/runner"
	"google.golang.org/protobuf/proto"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

type DefaultDaemon struct {
	Contexts   sync.Map
	AuthToken  string
	dispatcher *dispatch.Dispatcher
	compiles   singleflight.Group

	// sideEffectNoticeOnce fires the first time we bypass cache+dispatch
	// because the argv carries a flag that produces output files we
	// don't currently round-trip (-save-temps, -fdump-*, -gsplit-dwarf,
	// gcov instrumentation, …). One-shot per daemon lifetime so a build
	// using -save-temps on every TU doesn't spam the log — the user
	// reads it once on the first such compile and knows.
	sideEffectNoticeOnce sync.Once
}

// sideEffectBypassNotice is the one-time message the daemon prepends
// to the first local-invoke fallthrough whose argv triggered
// HasUncapturedSideEffectFlag. Yellow-ANSI to match the existing
// -Werror demotion notice in the dispatch package; same shape so the
// two reads consistently in build logs.
const sideEffectBypassNotice = "\033[33mhpcc: " +
	"this compile carries a flag (e.g. -save-temps, -gsplit-dwarf, " +
	"-fdump-*, --coverage) that writes output files alongside the .o " +
	"that hpcc can't currently round-trip through the cache/dispatch " +
	"path. Running locally so you get every file you asked for; this " +
	"TU and any like it won't hit the remote worker or the cache. " +
	"This notice appears once per daemon lifetime.\033[0m\n"

// NewDefaultDaemon loads the daemon's config and, if remote dispatch is
// enabled, prepares a Dispatcher. Failures to construct the dispatcher
// are logged but non-fatal — the daemon can still run as a local cache
// while the operator fixes the remote config.
func NewDefaultDaemon() *DefaultDaemon {
	d := &DefaultDaemon{Contexts: sync.Map{}}

	cfgPath := os.Getenv("HPCC_CONFIG")
	if cfgPath == "" {
		if p, err := config.DefaultConfigPath(); err == nil {
			cfgPath = p
		}
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Printf("daemon: load config: %v (continuing without remote dispatch)", err)
		return d
	}
	if cfg.Remote.Enabled {
		dp, err := dispatch.New(cfg.Remote, cfg.SourceMode)
		if err != nil {
			log.Printf("daemon: init remote dispatcher: %v (continuing local-only)", err)
		} else {
			d.dispatcher = dp
			log.Printf("daemon: remote dispatch enabled (scheduler=%s)", cfg.Remote.Scheduler.URL)
		}
	}
	return d
}

func setRunningDaemon(token string, port int) error {
	path := client.ConfigPath()
	if port < 1 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	cfg := &client.Config{
		Pid:       os.Getpid(),
		Port:      port,
		AuthToken: token,
	}

	bytes, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	return os.WriteFile(path, bytes, 0600)
}

func (d *DefaultDaemon) handleConnection(conn *net.TCPConn) error {
	remote := conn.RemoteAddr()
	log.Printf("connection(%s): accepted", remote)
	reader := bufio.NewReader(conn)
	var writeMu sync.Mutex

	if err := d.checkAuth(reader); err != nil {
		return fmt.Errorf("connection(%s): auth failed: %w", remote, err)
	}
	log.Printf("connection(%s): authenticated", remote)

	for {
		lengthBytes := make([]byte, 4)
		read, err := io.ReadFull(reader, lengthBytes)
		if err != nil || read != 4 {
			return fmt.Errorf("connection(%s): failed to read msg length %s", remote, err)
		}
		length := int(binary.BigEndian.Uint32(lengthBytes))
		if length == 0 {
			log.Printf("connection(%s): closed by client", remote)
			return nil
		}
		messageBytes := make([]byte, length)
		read, err = io.ReadFull(reader, messageBytes)
		if err != nil || read != length {
			return fmt.Errorf("connection(%s): failed to read msg %s", remote, err)
		}
		go d.handleRequest(messageBytes, conn, &writeMu)
	}
}

// checkAuth reads a length-prefixed token from the client and compares it
// to the daemon's AuthToken in constant time. An empty AuthToken on the
// daemon means auth is disabled (used by tests that exercise
// handleConnection directly without spinning up a real listener).
func (d *DefaultDaemon) checkAuth(reader *bufio.Reader) error {
	if d.AuthToken == "" {
		return nil
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return fmt.Errorf("read token length: %w", err)
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > 1024 {
		return fmt.Errorf("invalid token length %d", length)
	}
	token := make([]byte, length)
	if _, err := io.ReadFull(reader, token); err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	if subtle.ConstantTimeCompare(token, []byte(d.AuthToken)) != 1 {
		return fmt.Errorf("invalid token")
	}
	return nil
}

func (d *DefaultDaemon) writeResponse(conn *net.TCPConn, writeMu *sync.Mutex, data []byte) error {
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, uint32(len(data)))
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := conn.Write(lengthBytes); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

func (d *DefaultDaemon) writeErrorResponse(conn *net.TCPConn, writeMu *sync.Mutex, stderr string, exitCode int) {
	response := &gen.CompileResponse{
		Stderr:   []byte(stderr),
		ExitCode: int32(exitCode),
	}
	marshalled, err := proto.Marshal(response)
	if err != nil {
		log.Println(fmt.Errorf("marshal error response: %w", err))
		return
	}
	if err := d.writeResponse(conn, writeMu, marshalled); err != nil {
		log.Println(fmt.Errorf("write error response: %w", err))
	}
}

func (d *DefaultDaemon) getOrCreateContext(cmd string) (*compiler.Context, error) {
	if ctx, ok := d.Contexts.Load(cmd); ok && ctx != nil {
		return ctx.(*compiler.Context), nil
	}
	log.Printf("context: initializing %q", cmd)
	newCtx, err := runner.NewContext(cmd)
	if err != nil {
		return nil, err
	}
	actual, _ := d.Contexts.LoadOrStore(cmd, newCtx)
	return actual.(*compiler.Context), nil
}

// pathFlags lists separate-form flags whose next argv element is a
// filesystem path the daemon must resolve to the client's cwd.
var pathFlags = map[string]bool{
	"-o": true, "-I": true, "-L": true,
	"-isystem": true, "-iquote": true, "-isysroot": true,
	"-include": true, "-MF": true, "-MQ": true, "-MT": true,
}

// nonPathValueFlags lists separate-form flags whose next argv element
// is a *value* (language name, target triple, etc.) — NOT a path. The
// walker must skip past these without applying the
// "positional-arg → resolve" rule below, or it mangles values like
// `-x assembler-with-cpp` into `/cwd/assembler-with-cpp`. The kernel's
// scripts/as-version.sh probe was the case that surfaced this.
var nonPathValueFlags = map[string]bool{
	"-x":      true, // language: -x c, -x assembler-with-cpp, ...
	"-target": true, // clang target triple
}

func resolveRelativePaths(args []string, cwd string) []string {
	resolved := make([]string, len(args))
	copy(resolved, args)

	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cwd, p)
	}

	resolveNext := make(map[int]bool)
	skipNext := make(map[int]bool)
	for i, a := range resolved {
		switch {
		case skipNext[i]:
			// Value slot for a non-path flag; leave verbatim.
		case resolveNext[i]:
			resolved[i] = resolve(a)
		case pathFlags[a]:
			resolveNext[i+1] = true
		case nonPathValueFlags[a]:
			skipNext[i+1] = true
		case !strings.HasPrefix(a, "-"):
			resolved[i] = resolve(a)
		}
	}

	return resolved
}

func (d *DefaultDaemon) handleRequest(bytes []byte, conn *net.TCPConn, writeMu *sync.Mutex) {
	compileRequest := gen.CompileRequest{}

	if err := proto.Unmarshal(bytes, &compileRequest); err != nil {
		log.Println(fmt.Errorf("unmarshal: %w", err))
		d.writeErrorResponse(conn, writeMu, fmt.Sprintf("unmarshal: %v", err), 1)
		return
	}

	args := compileRequest.Args
	cmd := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	if cmd == "hpcc" {
		cmd = strings.TrimSuffix(filepath.Base(args[1]), ".exe")
		args = args[2:]
	} else {
		args = args[1:]
	}

	context, err := d.getOrCreateContext(cmd)
	if err != nil {
		log.Println(fmt.Errorf("new context: %w", err))
		d.writeErrorResponse(conn, writeMu, fmt.Sprintf("new context: %v", err), 1)
		return
	}

	if compileRequest.Cwd != "" {
		args = resolveRelativePaths(args, compileRequest.Cwd)
	}

	inv, err := context.Compiler.Parse(args)
	if err != nil {
		log.Println(fmt.Errorf("args_parse: %w", err))
		d.writeErrorResponse(conn, writeMu, fmt.Sprintf("args_parse: %v", err), 1)
		return
	}

	// Preserve the client's cwd so the spawned compiler resolves
	// joined-form `-Iinclude`, auto-derived depfiles, and any other
	// relative arguments against the directory the client ran in,
	// not the daemon's own cwd. resolveRelativePaths above already
	// absolutized known separate-form path flags; this catches
	// everything the walker can't reliably classify.
	inv.Cwd = compileRequest.Cwd

	log.Printf("compile: %s -> %s", cmd, inv.Output)

	// Two gates:
	//   - locallyCacheable: can the local CompileCache safely key on
	//     this invocation's preprocessed-bytes hash? False for
	//     assembly (the .incbin'd bytes aren't captured).
	//   - casDispatchable: can a CAS-mode worker handle this
	//     invocation end-to-end? True for assembly because the
	//     manifest captures the full closure.
	//
	// If both false → invocation is fundamentally unrepresentable
	// (link, multi-input, stdin, …); bypass cache and dispatch.
	// If only CAS-dispatchable → skip local cache, try dispatch,
	// fall back to local invoke without caching.
	// If both true → standard flow: local cache → dispatch → local
	// invoke fallback, with caching of remote/local results.
	locallyCacheable := inv.Cacheable()
	casDispatchable := d.dispatcher != nil &&
		d.dispatcher.SourceMode() == enum.SourceModeCAS &&
		inv.DispatchableUnderCAS()

	if !locallyCacheable && !casDispatchable {
		result, invokeErr := context.Compiler.Invoke(inv)
		if invokeErr != nil {
			log.Println(fmt.Errorf("compile: %w", invokeErr))
			d.writeErrorResponse(conn, writeMu, fmt.Sprintf("compile: %v", invokeErr), 1)
			return
		}
		// One-shot user-visible warning when the bypass is due to an
		// uncaptured-side-effect flag specifically (not the other
		// non-dispatchable cases like stdin / multi-input / link,
		// which are silently expected). Prepend to the compile's
		// stderr so it surfaces in the build log.
		if compiler.HasUncapturedSideEffectFlag(inv.RawArgs) {
			d.sideEffectNoticeOnce.Do(func() {
				if result != nil {
					result.Stderr = append([]byte(sideEffectBypassNotice), result.Stderr...)
				}
			})
		}
		d.writeCompileResult(conn, writeMu, inv, result)
		return
	}

	var hash string
	var hashErr error
	if locallyCacheable {
		hash, hashErr = inv.ComputeHash(context)
	}

	compile := func() (any, error) {
		if locallyCacheable {
			result, lookupErr := context.Cache.Lookup(inv)
			if lookupErr == nil && result != nil {
				log.Printf("compile: %s cache hit", inv.Output)
				return result, nil
			}
			log.Printf("compile: %s cache miss, invoking compiler", inv.Output)
		} else {
			// CAS-only path (e.g. .S inputs). Local cache is
			// unsound; the worker's manifest-keyed cache handles
			// hit detection on the remote side.
			log.Printf("compile: %s dispatching via CAS (local cache skipped)", inv.Output)
		}

		var fallbackWarning []byte
		if d.dispatcher != nil {
			remoteResult, remoteErr := d.dispatcher.Dispatch(context_pkgContext(), context.Compiler, inv)
			if remoteErr == nil {
				log.Printf("compile: %s served remotely (exit=%d)", inv.Output, remoteResult.ExitCode)
				if locallyCacheable {
					_ = context.Cache.Store(inv, remoteResult)
				}
				return remoteResult, nil
			}
			log.Printf("compile: %s remote dispatch failed: %v (falling back to local)", inv.Output, remoteErr)
			fallbackWarning = redWarning(remoteErr)
		}

		result, err := context.Compiler.Invoke(inv)
		if err != nil {
			return nil, err
		}
		if len(fallbackWarning) > 0 {
			result.Stderr = append(append([]byte{}, fallbackWarning...), result.Stderr...)
		}
		// Capture the user's dep-emission .d files (written by the
		// compiler's own -MMD/-MF side effect during Invoke above) into
		// result.Extras so the cache round-trips them. Without this,
		// a warm rebuild after `make clean` gets the .o restored from
		// cache but no .d, and tools that re-read the .d (kernel
		// `fixdep`, ninja's depfile parser) fail. Same shape as
		// dispatchPreprocessed's promotion.
		if extras := compiler.CollectDepEmissionExtras(inv); extras != nil {
			if result.Extras == nil {
				result.Extras = extras
			} else {
				for k, v := range extras {
					result.Extras[k] = v
				}
			}
		}
		if locallyCacheable {
			_ = context.Cache.Store(inv, result)
		}
		return result, nil
	}

	var val any
	var shared bool
	if hashErr != nil {
		val, err = compile()
	} else {
		val, err, shared = d.compiles.Do(hash, compile)
	}

	if err != nil {
		log.Println(fmt.Errorf("compile: %w", err))
		d.writeErrorResponse(conn, writeMu, fmt.Sprintf("compile: %v", err), 1)
		return
	}

	result := val.(*compiler.InvocationResult)
	if shared {
		log.Printf("compile: %s deduped", inv.Output)
	}
	d.writeCompileResult(conn, writeMu, inv, result)
}

// writeCompileResult writes the result of a compile (or link/preprocess/etc.)
// back to the client: spills the output blob to disk if one was produced,
// any side-effect extras (.d files etc.) to their cwd-relative paths,
// then sends a CompileResponse with stdout/stderr/exit code.
func (d *DefaultDaemon) writeCompileResult(conn *net.TCPConn, writeMu *sync.Mutex, inv *compiler.Invocation, result *compiler.InvocationResult) {
	log.Printf("compile: %s exit=%d", inv.Output, result.ExitCode)

	if inv.Output != "" && result.Output != nil {
		if err := os.WriteFile(inv.Output, result.Output, 0644); err != nil {
			log.Println(fmt.Errorf("write_output: %w", err))
		}
	}

	// Materialise side-effect outputs (.d files etc.) under inv.Cwd.
	// Single sink for both dispatch modes: dispatchCAS / dispatchPreprocessed
	// populate result.Extras and we write here. Critically this also
	// runs on cache-hit results — the cached extras blob replays the
	// .d file even though no preprocessor or compile actually ran for
	// this invocation, which is what keeps `make`'s incremental dep
	// tracking current across cache hits.
	for path, bytes := range result.Extras {
		full := path
		if !filepath.IsAbs(path) && inv.Cwd != "" {
			full = filepath.Join(inv.Cwd, path)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			log.Println(fmt.Errorf("write_extra: mkdir %q: %w", full, err))
			continue
		}
		if err := os.WriteFile(full, bytes, 0o644); err != nil {
			log.Println(fmt.Errorf("write_extra %q: %w", full, err))
		}
	}

	response := &gen.CompileResponse{
		Stdout:   result.Stdout,
		ExitCode: int32(result.ExitCode),
		Stderr:   result.Stderr,
	}

	marshalled, err := proto.Marshal(response)
	if err != nil {
		log.Println(fmt.Errorf("marshal: %w", err))
		return
	}
	if err := d.writeResponse(conn, writeMu, marshalled); err != nil {
		log.Println(fmt.Errorf("write: %w", err))
	}
}

// context_pkgContext returns a context for remote-dispatch RPCs. Kept
// as a function (rather than a parent context plumbed through
// handleRequest) so the rest of the daemon's connection lifecycle —
// which predates the dispatcher — doesn't need rethreading. Detached
// from the connection: a slow remote compile shouldn't be cancelled
// just because the client connection blipped.
func context_pkgContext() context.Context {
	return context.Background()
}

// redWarning formats a one-line ANSI-red note explaining that the
// remote compile failed and we fell back to local. Sent through the
// CompileResponse's stderr so the client process prints it to the
// user's terminal.
func redWarning(err error) []byte {
	const reset = "\033[0m"
	const red = "\033[31m"
	return []byte(red + "hpcc: remote dispatch failed (" + err.Error() + "); compiled locally" + reset + "\n")
}

func (d *DefaultDaemon) Run(force bool) error {
	if existing := client.Load(); !force && existing != nil {
		return fmt.Errorf("HPCC daemon process (%d) is already running, use --force only if you know what you're doing", existing.Pid)
	}

	d.AuthToken = rand.Text()

	var err error
	var a *net.TCPAddr
	if a, err = net.ResolveTCPAddr("tcp", "localhost:0"); err == nil {
		var l *net.TCPListener
		if l, err = net.ListenTCP("tcp", a); err == nil {
			defer func(l *net.TCPListener) {
				log.Printf("daemon: shutting down")
				_ = setRunningDaemon("", -1)
				_ = l.Close()
			}(l)
			port := l.Addr().(*net.TCPAddr).Port
			if err := setRunningDaemon(d.AuthToken, port); err != nil {
				return err
			}
			log.Printf("daemon: listening on localhost:%d (pid=%d)", port, os.Getpid())

			for {
				conn, err := l.AcceptTCP()
				if err != nil {
					log.Println(fmt.Errorf("accept: %w", err))
					continue
				}
				_ = conn.SetNoDelay(true)

				go func() {
					if err := d.handleConnection(conn); err != nil {
						log.Println(err)
					}
				}()
			}
		}
	}

	return nil
}
