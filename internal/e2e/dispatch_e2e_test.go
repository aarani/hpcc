//go:build e2e

// Package e2e exercises the full client→scheduler→worker dispatch path
// in-process: a fake IdP issues a real RS256 JWT, the scheduler verifies
// it via its JWKS endpoint, the worker authenticates with a static token
// and registers itself, the dispatcher does an OAuth password grant and
// runs a real compile against a host-side clang. Build-tagged "e2e" so
// `go test ./...` skips it; run with `go test -tags e2e ./internal/e2e`.
package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/golang-jwt/jwt/v5"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/daemon/dispatch"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/aarani/hpcc/internal/worker"
	wruntime "github.com/aarani/hpcc/internal/worker/runtime"
)

const (
	// Image identity advertised by the worker and requested by the
	// dispatcher. The "really_really_dangerous" runtime ignores the
	// digest at exec time, so any stable string works — but it must
	// be stable so scheduler.pickWorker matches.
	testImageDigest = "sha256:e2eaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testImageRef    = "ghcr.io/test/toolchain@" + testImageDigest

	testTenantID    = "tenant-test"
	testIssuer      = "https://test.idp.local/"
	testAudience    = "hpcc-scheduler"
	testWorkerToken = "static-worker-token-1234567890abcdef"
)

func TestE2E_DispatchClientToSchedulerToWorker(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skipf("clang not on PATH (%v); e2e test needs a real compiler", err)
	}

	// 1. Fake IdP — real RSA-2048 keypair, JSON Web Key Set served at
	// /.well-known/jwks.json, OAuth2 password grant at /token. The
	// scheduler will fetch the JWKS at NewScheduler time and use it
	// to verify our minted JWTs; the dispatcher will hit /token
	// during ensureSession.
	idpKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate IdP key: %v", err)
	}
	const idpKID = "test-key-1"
	idp := newFakeIdP(t, idpKey, idpKID)
	defer idp.Close()

	// 2. Self-signed TLS material for both servers. Includes IP SAN
	// 127.0.0.1 because we dial loopback by IP — DNS-only "localhost"
	// SANs would fail SNI verification.
	tmp := t.TempDir()
	schedCertFile, schedKeyFile := writeSelfSignedCert(t, tmp, "scheduler")
	workerCertFile, workerKeyFile := writeSelfSignedCert(t, tmp, "worker")

	// 3. Bind the scheduler listener up front so we know its address
	// before everything else that needs to be told about it.
	schedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind scheduler: %v", err)
	}
	schedAddr := schedListener.Addr().String()

	schedCfg := scheduler.Config{
		Listen: schedAddr,
		TLS: scheduler.TLSConfig{
			CertFile: schedCertFile,
			KeyFile:  schedKeyFile,
		},
		Auth: scheduler.Auth{
			WorkerToken: testWorkerToken,
		},
		Tenants: []scheduler.Tenant{{
			ID:       testTenantID,
			Issuer:   testIssuer,
			JWKSURL:  idp.URL + "/.well-known/jwks.json",
			TokenURL: idp.URL + "/token",
			Audience: testAudience,
			ClientID: "hpcc-test",
			Scope:    "hpcc",
		}},
		Routing: scheduler.Routing{StickyTenants: true},
	}
	sched, err := scheduler.NewScheduler(schedCfg)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	schedTLSCert, err := tls.LoadX509KeyPair(schedCertFile, schedKeyFile)
	if err != nil {
		t.Fatalf("load scheduler cert: %v", err)
	}
	schedSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{schedTLSCert},
		MinVersion:   tls.VersionTLS13,
	})))
	gen.RegisterSchedulerServiceServer(schedSrv, sched)
	go func() { _ = schedSrv.Serve(schedListener) }()
	defer schedSrv.GracefulStop()

	// 4. Same dance for the worker: bind first so we know its port,
	// then construct config (PublicAddr is read at register time).
	workerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	workerAddr := workerListener.Addr().String()

	workerCfg := worker.Config{
		Listen:     workerAddr,
		PublicAddr: workerAddr,
		TLS: worker.TLSConfig{
			CertFile: workerCertFile,
			KeyFile:  workerKeyFile,
		},
		Scheduler: worker.SchedulerLink{
			URL:         schedAddr,
			WorkerToken: testWorkerToken,
			CAFile:      schedCertFile,
		},
		Runtime: worker.RuntimeConfig{
			// dangerous handler runs compiles as host subprocesses
			// with /src ↔ tmpdir and /out ↔ tmpdir translation; this
			// is the only runtime that works without a real backend.
			Handler: wruntime.HandlerReallyReallyDangerous,
		},
		VM: worker.VMConfig{
			Memory:         "2GB",
			VCPUs:          4,
			IdleTimeout:    "10m",
			SessionTimeout: "8h",
		},
		Pool: worker.PoolConfig{MaxActive: 32},
		Image: worker.ImageConfig{
			AdvertisedDigests: []string{testImageDigest},
		},
	}

	w, err := worker.NewWorker(workerCfg)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	workerTLSCert, err := tls.LoadX509KeyPair(workerCertFile, workerKeyFile)
	if err != nil {
		t.Fatalf("load worker cert: %v", err)
	}
	workerSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{workerTLSCert},
		MinVersion:   tls.VersionTLS13,
	})))
	gen.RegisterWorkerServiceServer(workerSrv, w)
	go func() { _ = workerSrv.Serve(workerListener) }()
	defer workerSrv.GracefulStop()

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(runCtx) }()

	// 5. Dispatcher — what `hpcc start` would build at daemon startup
	// when remote.enabled = true.
	dispCfg := config.RemoteConfig{
		Enabled:     true,
		TenantID:    testTenantID,
		ImageRef:    testImageRef,
		ImageDigest: testImageDigest,
		Scheduler: config.SchedulerConfig{
			URL:    schedAddr,
			CAFile: schedCertFile,
		},
		OAuth: config.OAuthConfig{
			ClientSecret: "test-secret",
			Username:     "alice",
			Password:     "p4ssw0rd",
		},
	}
	disp, err := dispatch.New(dispCfg, enum.SourceModePreprocessed)
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}
	defer disp.Close()

	// 6. Wait for the worker's scheduler-side register to complete.
	// We don't have a direct hook, so probe by trying to compile in
	// a retry loop — Route returns "no available worker" until the
	// worker has registered with the right image digest.
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "hello.c")
	if err := os.WriteFile(srcFile, []byte("int square(int x){return x*x;}\n"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	objFile := filepath.Join(srcDir, "hello.o")

	c, err := compiler.Detect("clang")
	if err != nil {
		t.Fatalf("Detect clang: %v", err)
	}

	var (
		result   *compiler.InvocationResult
		lastErr  error
		deadline = time.Now().Add(15 * time.Second)
	)
	for time.Now().Before(deadline) {
		// Re-parse each iteration because Dispatch mutates argv via
		// RewriteForPreprocessed and we want a clean inv on retry.
		inv, err := c.Parse([]string{"-c", srcFile, "-o", objFile, "-O0"})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		result, lastErr = disp.Dispatch(context.Background(), c, inv)
		if lastErr == nil {
			break
		}
		if !strings.Contains(lastErr.Error(), "no available worker") {
			t.Fatalf("Dispatch: %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("Dispatch never succeeded within deadline: %v", lastErr)
	}

	// 7. Verify the round-trip produced a real object file.
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d; stderr=%q stdout=%q", result.ExitCode, result.Stderr, result.Stdout)
	}
	if len(result.Output) == 0 {
		t.Fatalf("OutputArtifact is empty; stderr=%q", result.Stderr)
	}
	if !looksLikeObjectFile(result.Output) {
		t.Fatalf("OutputArtifact does not start with an object-file magic; first bytes=%x", result.Output[:min(8, len(result.Output))])
	}
}

// --- fake IdP -------------------------------------------------------------

type fakeIdP struct {
	*httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newFakeIdP(t *testing.T, key *rsa.PrivateKey, kid string) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{key: key, kid: kid}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		jwk := map[string]any{
			"kty": "RSA",
			"kid": kid,
			"alg": "RS256",
			"use": "sig",
			"n":   b64url(key.N.Bytes()),
			"e":   b64url(big.NewInt(int64(key.E)).Bytes()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{jwk},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		// Don't enforce credentials in the test — the OAuth surface
		// here is only there to be exercised, not to be tested for
		// correctness.
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "password" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}

		claims := jwt.MapClaims{
			"iss": testIssuer,
			"aud": testAudience,
			"sub": r.Form.Get("username"),
			"iat": time.Now().Unix(),
			"nbf": time.Now().Unix(),
			"exp": time.Now().Add(10 * time.Minute).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		signed, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": signed,
			"token_type":   "Bearer",
			"expires_in":   600,
		})
	})

	idp.Server = httptest.NewServer(mux)
	return idp
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- self-signed cert generator ------------------------------------------

func writeSelfSignedCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// --- output sanity check -------------------------------------------------

// looksLikeObjectFile checks the magic bytes for ELF (Linux), Mach-O
// (macOS), or COFF (Windows). One of the three should always be the
// platform clang's output format.
func looksLikeObjectFile(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	// ELF
	if b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F' {
		return true
	}
	// Mach-O 32/64, both endiannesses
	switch {
	case b[0] == 0xfe && b[1] == 0xed && b[2] == 0xfa && (b[3] == 0xce || b[3] == 0xcf):
		return true
	case b[0] == 0xcf && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe:
		return true
	case b[0] == 0xce && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe:
		return true
	}
	// COFF (Windows .obj): machine type word; common values are
	// 0x8664 (AMD64) and 0x014c (i386) at offset 0.
	if (b[0] == 0x64 && b[1] == 0x86) || (b[0] == 0x4c && b[1] == 0x01) {
		return true
	}
	return false
}

// silence linter on unused error sentinel (kept for future expansion).
var _ = errors.New
var _ = fmt.Sprintf
