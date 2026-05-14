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
}

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
		dp, err := dispatch.New(cfg.Remote)
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

var pathFlags = map[string]bool{
	"-o": true, "-I": true, "-L": true,
	"-isystem": true, "-iquote": true, "-isysroot": true,
	"-include": true, "-MF": true, "-MQ": true, "-MT": true,
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
	for i, a := range resolved {
		if resolveNext[i] {
			resolved[i] = resolve(a)
		} else if pathFlags[a] {
			resolveNext[i+1] = true
		} else if !strings.HasPrefix(a, "-") {
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

	log.Printf("compile: %s -> %s", cmd, inv.Output)

	// Non-compile invocations (link, preprocess-only, dep-only, assemble)
	// bypass the cache and remote dispatch entirely: their inputs and
	// outputs aren't safe to cache by source-hash, and remote workers
	// don't have the local library/object state they'd need.
	if inv.Mode != enum.CompileMode {
		result, invokeErr := context.Compiler.Invoke(inv)
		if invokeErr != nil {
			log.Println(fmt.Errorf("compile: %w", invokeErr))
			d.writeErrorResponse(conn, writeMu, fmt.Sprintf("compile: %v", invokeErr), 1)
			return
		}
		d.writeCompileResult(conn, writeMu, inv, result)
		return
	}

	hash, hashErr := inv.ComputeHash(context)

	compile := func() (any, error) {
		result, lookupErr := context.Cache.Lookup(inv)
		if lookupErr == nil && result != nil {
			log.Printf("compile: %s cache hit", inv.Output)
			return result, nil
		}
		log.Printf("compile: %s cache miss, invoking compiler", inv.Output)

		var fallbackWarning []byte
		if d.dispatcher != nil {
			remoteResult, remoteErr := d.dispatcher.Dispatch(context_pkgContext(), context.Compiler, inv)
			if remoteErr == nil {
				log.Printf("compile: %s served remotely (exit=%d)", inv.Output, remoteResult.ExitCode)
				_ = context.Cache.Store(inv, remoteResult)
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
		_ = context.Cache.Store(inv, result)
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
// then sends a CompileResponse with stdout/stderr/exit code.
func (d *DefaultDaemon) writeCompileResult(conn *net.TCPConn, writeMu *sync.Mutex, inv *compiler.Invocation, result *compiler.InvocationResult) {
	log.Printf("compile: %s exit=%d", inv.Output, result.ExitCode)

	if inv.Output != "" && result.Output != nil {
		if err := os.WriteFile(inv.Output, result.Output, 0644); err != nil {
			log.Println(fmt.Errorf("write_output: %w", err))
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
