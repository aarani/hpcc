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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mostynb/go-grpc-compression/zstd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/protocol/gen"
)

// Dispatcher is the daemon-side handle to the scheduler+worker mesh.
// One per daemon process; goroutine-safe. Dial happens once at New;
// session tokens are refreshed on demand.
type Dispatcher struct {
	cfg config.RemoteConfig

	schedConn *grpc.ClientConn
	sched     gen.SchedulerServiceClient

	sessionMu    sync.Mutex
	sessionToken string

	workersMu sync.Mutex
	workers   map[string]*workerHandle
}

type workerHandle struct {
	conn   *grpc.ClientConn
	client gen.WorkerServiceClient
}

// New dials the scheduler and returns a ready Dispatcher. The scheduler
// connection is lazy at the gRPC layer (no actual TCP until first RPC),
// so a misconfigured scheduler URL surfaces on the first compile, not
// at daemon startup.
func New(cfg config.RemoteConfig) (*Dispatcher, error) {
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
		cfg:       cfg,
		schedConn: conn,
		sched:     gen.NewSchedulerServiceClient(conn),
		workers:   map[string]*workerHandle{},
	}, nil
}

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
	return result, nil
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
		grpc.WithDefaultCallOptions(grpc.UseCompressor(zstd.Name)),
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
