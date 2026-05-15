// Package dispatch implements the daemon's remote-compile path: get a
// JWT from the configured IdP via OAuth password grant, authenticate
// against the scheduler, route each compile to a worker, and dial the
// worker directly to invoke Compile. Lives in its own package so the
// daemon doesn't grow a tangle of grpc/TLS/oauth wiring inline.
package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mostynb/go-grpc-compression/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
)

// Dispatcher is the daemon-side handle to the scheduler+worker mesh.
// One per daemon process; goroutine-safe. Dial happens once at New;
// session tokens are refreshed on demand.
type Dispatcher struct {
	cfg        config.RemoteConfig
	sourceMode enum.SourceMode

	schedConn *grpc.ClientConn
	sched     gen.SchedulerServiceClient

	sessionMu    sync.Mutex
	sessionToken string

	workersMu sync.Mutex
	workers   map[string]*workerHandle

	// werrorNoticeOnce fires once per dispatcher lifetime, on the
	// first successful remote dispatch that had `-Werror[=*]` in its
	// argv. We prepend a one-time yellow notice to that compile's
	// stderr so the user — reading their build log — sees why a
	// previously-fatal warning now comes back as a warning. Without
	// this, the demotion is silent and confusing.
	werrorNoticeOnce sync.Once
}

// werrorDemotionNotice is the one-time message the dispatcher emits
// on the first remote compile whose argv contained -Werror[=*]. It
// gets prepended to that compile's stderr (which surfaces in the
// user's build log) so the strip is auditable rather than silent.
// Wrapped in yellow ANSI for visibility — the daemon already uses
// the same shape for its fallback-to-local warning.
const werrorDemotionNotice = "\033[33mhpcc: " +
	"-Werror[=…] flags are demoted to plain warnings in remote " +
	"(preprocessed-mode) compiles. gcc's macro-expansion warning " +
	"suppression only works in one-step compiles, so honoring -Werror " +
	"across the preprocess/compile split would fail builds on code that " +
	"local-mode gcc accepts. Warnings still emit; this notice appears " +
	"once per daemon lifetime.\033[0m\n"

type workerHandle struct {
	conn   *grpc.ClientConn
	client gen.WorkerServiceClient
}

// New dials the scheduler and returns a ready Dispatcher. The scheduler
// connection is lazy at the gRPC layer (no actual TCP until first RPC),
// so a misconfigured scheduler URL surfaces on the first compile, not
// at daemon startup.
//
// sourceMode is taken from the top-level config (it drives both the
// dispatch wire format AND the local cache key — see enum.SourceMode);
// it's threaded in here rather than read off RemoteConfig because the
// field lives on the parent Config now.
func New(cfg config.RemoteConfig, sourceMode enum.SourceMode) (*Dispatcher, error) {
	if cfg.Scheduler.URL == "" {
		return nil, fmt.Errorf("remote.scheduler.url is required when remote is enabled")
	}

	tlsCfg := &tls.Config{}
	if cfg.Scheduler.CAFile != "" {
		pemBytes, err := os.ReadFile(cfg.Scheduler.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read scheduler CA %q: %w", cfg.Scheduler.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("scheduler CA %q contains no PEM certs", cfg.Scheduler.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	conn, err := grpc.NewClient(
		cfg.Scheduler.URL,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithDefaultCallOptions(grpc.UseCompressor(zstd.Name)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial scheduler: %w", err)
	}

	return &Dispatcher{
		cfg:        cfg,
		sourceMode: sourceMode,
		schedConn:  conn,
		sched:      gen.NewSchedulerServiceClient(conn),
		workers:    map[string]*workerHandle{},
	}, nil
}

// SourceMode reports the dispatcher's configured source-staging
// strategy (preprocessed or CAS). The daemon reads this to widen
// the dispatch gate for invocations that PREPROCESSED can't safely
// handle (e.g. GAS .S with .incbin — see Cacheable's comments).
func (d *Dispatcher) SourceMode() enum.SourceMode { return d.sourceMode }

// Close shuts down all gRPC connections (scheduler + worker pool).
func (d *Dispatcher) Close() error {
	d.workersMu.Lock()
	for k, w := range d.workers {
		_ = w.conn.Close()
		delete(d.workers, k)
	}
	d.workersMu.Unlock()
	if d.schedConn != nil {
		return d.schedConn.Close()
	}
	return nil
}

// Dispatch runs a single compile remotely: preprocess locally, route
// via scheduler, send to the worker. The returned InvocationResult is
// shaped exactly like a local compile so the caller can plug it into
// the same cache/write/return path.
//
// On any remote-side error (auth, route, dial, RPC) the session token
// is cleared (next call re-authenticates) and the error is returned —
// the caller is responsible for falling back to local execution.
func (d *Dispatcher) Dispatch(ctx context.Context, c compiler.Compiler, inv *compiler.Invocation) (*compiler.InvocationResult, error) {
	if d.sourceMode == enum.SourceModeCAS {
		return d.dispatchCAS(ctx, c, inv)
	}
	return d.dispatchPreprocessed(ctx, c, inv)
}

// dispatchPreprocessed is the v1 default path: preprocess locally,
// ship preprocessed bytes inline in CompileRequest. Pre-existing code
// preserved verbatim; the CAS branch is additive.
func (d *Dispatcher) dispatchPreprocessed(ctx context.Context, c compiler.Compiler, inv *compiler.Invocation) (*compiler.InvocationResult, error) {
	pp, err := c.Preprocess(inv)
	if err != nil {
		return nil, fmt.Errorf("preprocess: %w", err)
	}
	if pp.ExitCode != 0 {
		// Preprocessor failed — surface as a compile failure, no
		// point shipping garbage bytes to the worker.
		return &compiler.InvocationResult{
			Stderr:   pp.Stderr,
			ExitCode: pp.ExitCode,
		}, nil
	}

	// In-container paths. The worker's stage-source step writes our
	// PreprocessedSource bytes to /src/main.i (filename hardcoded in
	// worker/staging.go) and bind-mounts an empty /out for artifacts.
	// argv must reference both — rewrite the output path to /out/<base>
	// before the preprocessed-rewrite picks it up.
	srcPath := "/src/main.i"

	prepared := *inv
	if prepared.Output != "" {
		prepared.Output = "/out/" + filepath.Base(prepared.Output)
	}

	rewritten, err := c.RewriteForPreprocessed(&prepared, srcPath)
	if err != nil {
		return nil, fmt.Errorf("rewrite for preprocessed: %w", err)
	}

	if err := d.ensureSession(ctx); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	route, err := d.route(ctx)
	if err != nil {
		// Most likely the session is stale (scheduler restart). Drop
		// it; the next compile will re-authenticate.
		d.dropSession()
		return nil, fmt.Errorf("route: %w", err)
	}

	workerClient, err := d.workerClient(route.WorkerAddress, route.CertFingerprint)
	if err != nil {
		return nil, fmt.Errorf("dial worker %s: %w", route.WorkerAddress, err)
	}

	req := &gen.CompileRequest{
		Args: append([]string{c.Name()}, rewritten.RawArgs...),
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       d.cfg.TenantID,
			ImageDigest:    d.cfg.ImageDigest,
			ImageRef:       d.cfg.ImageRef,
			SchedulerToken: route.Token,
			SourceMode:     gen.SourceMode_PREPROCESSED,
			SourceSettings: &gen.RemoteDescriptor_Preprocessed{
				Preprocessed: &gen.PreprocessedDescriptor{
					PreprocessedSource: pp.Source,
				},
			},
		},
	}

	resp, err := workerClient.Compile(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("worker Compile RPC: %w", err)
	}

	result := &compiler.InvocationResult{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: int(resp.ExitCode),
	}
	if len(resp.OutputArtifact) > 0 {
		result.Output = resp.OutputArtifact
	}

	// Promote the cpp-side-effect .d files into result.Extras so they
	// flow through the same cache + writeback pipeline as CAS-mode
	// extras. The client-side Preprocess pass above ran with the user's
	// -Wp,-MMD,<path> flags intact and wrote the .d file(s) to the
	// requested paths under inv.Cwd; we just capture them here. After
	// this returns, the daemon's writeCompileResult materialises
	// result.Extras (so cache HITS on later compiles replay the .d
	// file even though no cpp ran — same property CAS gets from the
	// worker shipping extras back over the wire).
	if depPaths := compiler.ExtractDepEmissionPaths(inv.RawArgs); len(depPaths) > 0 {
		extras := make(map[string][]byte, len(depPaths))
		for _, p := range depPaths {
			full := p
			if !filepath.IsAbs(p) && inv.Cwd != "" {
				full = filepath.Join(inv.Cwd, p)
			}
			b, err := os.ReadFile(full)
			if err != nil {
				// The cpp pass may have produced no .d for this path
				// (e.g. -MMD on a no-include source); not an error,
				// just nothing to cache for that entry.
				continue
			}
			extras[p] = b
		}
		if len(extras) > 0 {
			result.Extras = extras
		}
	}

	// One-shot user-visible notice: if the user's argv carried
	// -Werror[=…] and we just stripped it across the rewrite seam,
	// prepend an explanation to this compile's stderr so it lands
	// in the build log. Subsequent compiles in the same daemon
	// lifetime don't repeat it.
	if hadWerror(inv.RawArgs) {
		d.werrorNoticeOnce.Do(func() {
			result.Stderr = append([]byte(werrorDemotionNotice), result.Stderr...)
		})
	}

	return result, nil
}

// dispatchCAS is the CAS-mode path (docs/cas.md). Builds a content-
// addressed manifest of the source closure and asks the worker if the
// resulting compile cache key is already a hit (1-RPC short-circuit,
// the headline incremental-build + cross-developer win). On miss,
// streams missing blobs and runs the full CAS compile.
func (d *Dispatcher) dispatchCAS(ctx context.Context, c compiler.Compiler, inv *compiler.Invocation) (*compiler.InvocationResult, error) {
	cctx := &compiler.Context{Compiler: c}
	manifest, err := compiler.BuildManifest(inv, cctx)
	if err != nil {
		return nil, fmt.Errorf("build manifest: %w", err)
	}

	if err := d.ensureSession(ctx); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	route, err := d.route(ctx)
	if err != nil {
		d.dropSession()
		return nil, fmt.Errorf("route: %w", err)
	}

	workerClient, err := d.workerClient(route.WorkerAddress, route.CertFingerprint)
	if err != nil {
		return nil, fmt.Errorf("dial worker %s: %w", route.WorkerAddress, err)
	}

	// Probe first. Cache hit → return immediately; no source upload.
	probe := &gen.CompileProbe{
		ManifestDigest: manifest.Digest[:],
		Args:           append([]string{c.Name()}, inv.RawArgs...),
		TenantId:       d.cfg.TenantID,
		ImageDigest:    d.cfg.ImageDigest,
		SchedulerToken: route.Token,
	}
	resp, err := workerClient.ProbeCompileCache(ctx, probe)
	if err != nil {
		return nil, fmt.Errorf("worker ProbeCompileCache RPC: %w", err)
	}
	if hit := resp.GetHit(); hit != nil {
		return compileResponseToResult(hit), nil
	}

	// Probe miss: run the upload dance, then Compile.
	if err := d.casUpload(ctx, workerClient, manifest, inv); err != nil {
		return nil, fmt.Errorf("cas upload: %w", err)
	}

	projectRoot := ""
	if inv.Cwd != "" {
		projectRoot = compiler.FindProjectRoot(inv.Cwd)
	}
	if projectRoot == "" {
		// No .hpcc marker means BuildManifest produced absolute
		// paths, and the worker has no project root to materialize
		// against. Refuse rather than ship a broken Compile.
		return nil, fmt.Errorf("CAS dispatch requires a .hpcc project marker; none found above %q", inv.Cwd)
	}

	rewritten := compiler.RewriteForCAS(inv, projectRoot, "/src", "/out")
	// Rewrite dep-emission paths (-Wp,-MMD,X / -MF X) to live under
	// /out so the worker writes the resulting .d files where the
	// agent already auto-streams from. The list of original paths
	// drives the client-side write-back after the response.
	finalArgs, extraOutputPaths := compiler.RewriteDepEmissionForCAS(rewritten.RawArgs, "/out")
	entryPath, err := projectRelative(projectRoot, inv.Inputs[0])
	if err != nil {
		return nil, fmt.Errorf("compute entry_path: %w", err)
	}

	req := &gen.CompileRequest{
		Args: append([]string{c.Name()}, finalArgs...),
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       d.cfg.TenantID,
			ImageDigest:    d.cfg.ImageDigest,
			ImageRef:       d.cfg.ImageRef,
			SchedulerToken: route.Token,
			SourceMode:     gen.SourceMode_CAS,
			SourceSettings: &gen.RemoteDescriptor_Cas{
				Cas: &gen.CasDescriptor{
					ManifestDigest: manifest.Digest[:],
					Blobs:          manifestBlobsToProto(manifest.Blobs),
					EntryPath:      entryPath,
				},
			},
		},
	}

	compileResp, err := workerClient.Compile(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("worker Compile RPC: %w", err)
	}

	result := compileResponseToResult(compileResp)
	// Promote side-effect outputs (.d files) into result.Extras so
	// the daemon's writeCompileResult is the single place that
	// materialises them on disk. Iterating the *requested* paths
	// rather than the raw response map keeps a buggy/hostile worker
	// from inserting extra entries that would land on the client's
	// filesystem under inv.Cwd. The /out-relative path the worker
	// keyed by matches the cwd-relative path the user passed.
	if len(extraOutputPaths) > 0 {
		filtered := make(map[string][]byte, len(extraOutputPaths))
		for _, p := range extraOutputPaths {
			if b, ok := compileResp.ExtraOutputs[p]; ok {
				filtered[p] = b
			}
		}
		if len(filtered) > 0 {
			result.Extras = filtered
		}
	}
	return result, nil
}

// casUpload runs the FindMissingBlobs + UploadBlobs handshake for the
// project-relative slice of the manifest's blob list. System (absolute)
// paths are skipped — those files live in the toolchain image, not in
// the source closure the worker materialises.
func (d *Dispatcher) casUpload(ctx context.Context, workerClient gen.WorkerServiceClient, manifest *compiler.Manifest, inv *compiler.Invocation) error {
	projectBlobs := projectBlobs(manifest.Blobs)
	if len(projectBlobs) == 0 {
		return nil
	}

	// FindMissingBlobs: stream the digests we'd like to ship; collect
	// the subset the worker doesn't already have.
	probeStream, err := workerClient.FindMissingBlobs(ctx)
	if err != nil {
		return fmt.Errorf("open FindMissingBlobs: %w", err)
	}
	sendDone := make(chan error, 1)
	go func() {
		for _, b := range projectBlobs {
			if err := probeStream.Send(&gen.BlobDigest{Digest: b.Digest[:], Size: uint64(b.Size)}); err != nil {
				sendDone <- fmt.Errorf("send probe: %w", err)
				return
			}
		}
		sendDone <- probeStream.CloseSend()
	}()
	missing := map[[32]byte]struct{}{}
	for {
		m, err := probeStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("recv probe: %w", err)
		}
		var d [32]byte
		copy(d[:], m.Digest)
		missing[d] = struct{}{}
	}
	if err := <-sendDone; err != nil {
		return err
	}

	if len(missing) == 0 {
		return nil
	}

	// UploadBlobs: stream header+data for each missing blob. Path-by-
	// disk content; we re-resolve from the BlobRef's path against the
	// project root.
	upStream, err := workerClient.UploadBlobs(ctx)
	if err != nil {
		return fmt.Errorf("open UploadBlobs: %w", err)
	}
	projectRoot := compiler.FindProjectRoot(inv.Cwd)
	for _, b := range projectBlobs {
		if _, want := missing[b.Digest]; !want {
			continue
		}
		full := filepath.Join(projectRoot, filepath.FromSlash(b.Path))
		if err := streamUpload(upStream, b, full); err != nil {
			return err
		}
	}
	result, err := upStream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("close UploadBlobs: %w", err)
	}
	if len(result.RejectedDigests) > 0 {
		return fmt.Errorf("worker rejected %d uploaded blob(s)", len(result.RejectedDigests))
	}
	return nil
}

// streamUpload sends one blob over the UploadBlobs stream: a header
// carrying the claimed digest, then chunks of file content sized to
// fit comfortably inside one gRPC frame.
func streamUpload(stream gen.WorkerService_UploadBlobsClient, ref compiler.BlobRef, path string) error {
	if err := stream.Send(&gen.BlobChunk{
		Body: &gen.BlobChunk_Header{Header: &gen.BlobDigest{Digest: ref.Digest[:], Size: uint64(ref.Size)}},
	}); err != nil {
		return fmt.Errorf("send header for %q: %w", ref.Path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	buf := make([]byte, 64*1024) // 64 KiB chunks; HTTP/2 framing handles flow control
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&gen.BlobChunk{Body: &gen.BlobChunk_Data{Data: buf[:n]}}); err != nil {
				return fmt.Errorf("send data for %q: %w", ref.Path, err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read %q: %w", path, rerr)
		}
	}
	return nil
}

// projectBlobs returns the subset of the manifest's blobs that need
// uploading: anything whose path is project-relative (not absolute).
// Absolute paths are system paths — the file is in the toolchain
// image, not in the source closure.
func projectBlobs(blobs []compiler.BlobRef) []compiler.BlobRef {
	out := make([]compiler.BlobRef, 0, len(blobs))
	for _, b := range blobs {
		if !filepath.IsAbs(b.Path) {
			out = append(out, b)
		}
	}
	return out
}

// manifestBlobsToProto converts compiler.BlobRef → gen.BlobRef. The
// shapes are identical except for the digest representation ([32]byte
// vs []byte).
func manifestBlobsToProto(blobs []compiler.BlobRef) []*gen.BlobRef {
	out := make([]*gen.BlobRef, len(blobs))
	for i, b := range blobs {
		out[i] = &gen.BlobRef{
			Digest: append([]byte(nil), b.Digest[:]...),
			Path:   b.Path,
			Size:   uint64(b.Size),
		}
	}
	return out
}

// projectRelative returns p re-expressed relative to projectRoot.
// Used to compute the CasDescriptor.entry_path that names the TU root
// within the source closure.
func projectRelative(projectRoot, p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(projectRoot, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry path %q escapes project root %q", p, projectRoot)
	}
	return filepath.ToSlash(rel), nil
}

// compileResponseToResult unwraps a worker CompileResponse into the
// runner-shaped InvocationResult the daemon caller expects.
func compileResponseToResult(resp *gen.CompileResponse) *compiler.InvocationResult {
	r := &compiler.InvocationResult{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: int(resp.ExitCode),
	}
	if len(resp.OutputArtifact) > 0 {
		r.Output = resp.OutputArtifact
	}
	return r
}

// hadWerror reports whether argv contained a -Werror or -Werror=<class>
// flag — i.e., whether the rewrite seam stripped something user-visible.
// Used to decide whether to fire the one-shot demotion notice.
func hadWerror(args []string) bool {
	for _, a := range args {
		if a == "-Werror" || strings.HasPrefix(a, "-Werror=") {
			return true
		}
	}
	return false
}

// ensureSession returns nil when d.sessionToken is set. On a cold
// session it fetches an OAuth access token via password grant and
// trades it for a scheduler session token.
func (d *Dispatcher) ensureSession(ctx context.Context) error {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()

	if d.sessionToken != "" {
		return nil
	}

	jwt, err := fetchOAuthToken(ctx, d.cfg.OAuth)
	if err != nil {
		return fmt.Errorf("oauth password grant: %w", err)
	}

	resp, err := d.sched.Authenticate(ctx, &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: jwt},
	})
	if err != nil {
		return fmt.Errorf("scheduler Authenticate RPC: %w", err)
	}
	if !resp.Success || resp.SessionToken == "" {
		return fmt.Errorf("scheduler rejected JWT")
	}
	d.sessionToken = resp.SessionToken
	return nil
}

func (d *Dispatcher) dropSession() {
	d.sessionMu.Lock()
	d.sessionToken = ""
	d.sessionMu.Unlock()
}

func (d *Dispatcher) route(ctx context.Context) (*gen.RouteResponse, error) {
	d.sessionMu.Lock()
	session := d.sessionToken
	d.sessionMu.Unlock()
	return d.sched.Route(ctx, &gen.RouteRequest{
		SessionToken: session,
		TenantId:     d.cfg.TenantID,
		ImageDigest:  d.cfg.ImageDigest,
	})
}

// workerClient returns a gRPC client for the worker at addr, pinned
// against the SHA-256 fingerprint registered with the scheduler.
// Connections are reused across compiles — gRPC over HTTP/2 multiplexes
// hundreds of in-flight RPCs onto one TCP connection.
func (d *Dispatcher) workerClient(addr string, fingerprint []byte) (gen.WorkerServiceClient, error) {
	key := addr // fingerprint is part of the cache value, not the key — addr is unique per worker
	d.workersMu.Lock()
	defer d.workersMu.Unlock()
	if h, ok := d.workers[key]; ok {
		return h.client, nil
	}

	pinned := fingerprint
	tlsCfg := &tls.Config{
		// We pin on the cert fingerprint, not the CA chain — workers
		// run with self-signed certs and the scheduler is the source
		// of truth for "this is the cert you should see."
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("worker presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(sum[:], pinned) != 1 {
				return fmt.Errorf("worker cert fingerprint mismatch")
			}
			return nil
		},
	}
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithDefaultCallOptions(
			grpc.UseCompressor(zstd.Name),
			// The unary worker.Compile RPC carries preprocessed
			// source in the request and the compiled object in
			// the response. Both can exceed gRPC's default 4 MiB
			// — kernel TUs preprocess to 5-15 MiB routinely. Match
			// the worker server's MaxRecvMsgSize so the call goes
			// through in both directions.
			grpc.MaxCallRecvMsgSize(gen.MaxCompileMessageBytes),
			grpc.MaxCallSendMsgSize(gen.MaxCompileMessageBytes),
		),
	)
	if err != nil {
		return nil, err
	}
	h := &workerHandle{conn: conn, client: gen.NewWorkerServiceClient(conn)}
	d.workers[key] = h
	return h.client, nil
}

// fetchOAuthToken runs an RFC 6749 §4.3 password grant. Form-encoded
// POST, JSON response. We pull only access_token; refresh tokens
// aren't useful here because the scheduler session token is what gets
// reused — when it dies, we re-run the whole grant.
func fetchOAuthToken(ctx context.Context, oc config.OAuthConfig) (string, error) {
	if oc.TokenURL == "" {
		return "", fmt.Errorf("remote.oauth.token_url is required")
	}
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", oc.Username)
	form.Set("password", oc.Password)
	if oc.ClientID != "" {
		form.Set("client_id", oc.ClientID)
	}
	if oc.ClientSecret != "" {
		form.Set("client_secret", oc.ClientSecret)
	}
	if oc.Scope != "" {
		form.Set("scope", oc.Scope)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oc.TokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, truncate(body, 256))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if parsed.AccessToken == "" {
		if parsed.Error != "" {
			return "", fmt.Errorf("oauth error %q: %s", parsed.Error, parsed.ErrorDesc)
		}
		return "", fmt.Errorf("token endpoint returned no access_token")
	}
	return parsed.AccessToken, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
