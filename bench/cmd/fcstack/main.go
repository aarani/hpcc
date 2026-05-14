// Command fcstack boots an end-to-end hpcc Phase 4 stack in a single
// process so the kernel-build benchmark has a target to dispatch
// against. It is the supervised peer of bench/kernel-bench-fc.sh.
//
// What it does on startup:
//
//   - generates self-signed TLS material for both gRPC servers
//   - generates an RSA keypair and starts a fake IdP (JWKS +
//     /token OAuth2 password grant) on a loopback HTTP listener
//   - builds the in-VM hpcc-agent for linux/amd64
//   - pulls the configured OCI toolchain image and runs the rootfs
//     pipeline (rootfs.Store.PullImage) to materialize a prepared
//     squashfs in --rootfs-dir, with /.hpcc/agent injected
//   - constructs an in-process scheduler.Scheduler and starts its
//     gRPC server
//   - constructs an in-process worker.Worker (handler=firecracker,
//     pointing at the firecracker/jailer binaries and the test
//     kernel from --firecracker-bin / --jailer-bin / --kernel),
//     starts its gRPC server, and begins its scheduler-register
//     liaison loop
//   - writes a client TOML at --client-config pointing at the
//     scheduler with remote.enabled = true, then prints the path
//     of that file on stdout (terminated by a newline) so the
//     orchestrating shell script knows what to set HPCC_CONFIG to
//
// Then it parks on SIGINT/SIGTERM, gracefully shuts the gRPC
// servers, and exits.
//
// Requires root (jailer's CAP_SYS_ADMIN setup + chroot). The shell
// wrapper handles that by invoking under `sudo -E`.
package main

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
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/aarani/hpcc/internal/worker"
	"github.com/aarani/hpcc/internal/worker/image/rootfs"
	wruntime "github.com/aarani/hpcc/internal/worker/runtime"
)

// IdP constants are stable strings; the scheduler validates issuer
// and audience exactly, and the access-token sub is irrelevant to
// the routing layer.
const (
	idpIssuer   = "https://fcstack.idp.local/"
	idpAudience = "hpcc-scheduler"

	// Shared worker token. Long enough to satisfy worker.Validate
	// (>= 16 chars) without being so unique that misconfig is
	// invisible if it leaks into a log.
	workerToken = "fcstack-worker-token-aaaaaaaaaa"

	tenantID = "bench"
)

func main() {
	var (
		stackDir   = flag.String("stack-dir", "", "scratch dir for certs/agent/rootfs (required)")
		clientCfg  = flag.String("client-config", "", "where to write the client TOML (required)")
		fcBin      = flag.String("firecracker-bin", os.Getenv("HPCC_FIRECRACKER_BIN"), "firecracker binary path")
		jailerBin  = flag.String("jailer-bin", os.Getenv("HPCC_JAILER_BIN"), "jailer binary path")
		kernel     = flag.String("kernel", os.Getenv("HPCC_TEST_KERNEL"), "vmlinux for the microVMs")
		imageRef   = flag.String("image-ref", "cgr.dev/chainguard/gcc-glibc:latest-dev", "OCI toolchain image")
		schedBind  = flag.String("scheduler-listen", "127.0.0.1:0", "scheduler bind address")
		workerBind = flag.String("worker-listen", "127.0.0.1:0", "worker bind address")
		idpBind    = flag.String("idp-listen", "127.0.0.1:0", "IdP HTTP bind address")
		vmMem      = flag.String("vm-memory", "2GB", "per-VM memory")
		vmVCPUs    = flag.Int("vm-vcpus", 2, "per-VM vCPUs")
		poolMax    = flag.Int("pool-max-active", 8, "max concurrent VMs per tenant")
	)
	flag.Parse()

	if *stackDir == "" || *clientCfg == "" {
		log.Fatalf("--stack-dir and --client-config are required")
	}
	if *fcBin == "" || *jailerBin == "" || *kernel == "" {
		log.Fatalf("--firecracker-bin, --jailer-bin, --kernel are required (env HPCC_FIRECRACKER_BIN/HPCC_JAILER_BIN/HPCC_TEST_KERNEL)")
	}
	if os.Geteuid() != 0 {
		log.Fatalf("fcstack: must run as root (jailer needs CAP_SYS_ADMIN + chroot)")
	}

	if err := os.MkdirAll(*stackDir, 0o755); err != nil {
		log.Fatalf("mkdir stack-dir: %v", err)
	}

	// 1. IdP — bring it up first so we have a JWKS URL to hand to
	//    the scheduler before it validates the first user JWT.
	idpKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("rsa keygen: %v", err)
	}
	idpURL, idpStop, err := startIdP(*idpBind, idpKey, "fcstack-key-1")
	if err != nil {
		log.Fatalf("start IdP: %v", err)
	}
	defer idpStop()
	log.Printf("fcstack: IdP at %s", idpURL)

	// 2. TLS material. Same cert serves both gRPC endpoints — the
	//    scheduler pins by SHA-256 of the cert anyway, and there's
	//    no SNI distinction worth making in a single-host bench.
	certPath, keyPath, err := writeSelfSignedCert(*stackDir, "fcstack")
	if err != nil {
		log.Fatalf("write cert: %v", err)
	}

	// 3. Bind both gRPC listeners up front; we need their addresses
	//    before we can configure the scheduler-side worker register
	//    or the client TOML.
	schedLis, err := net.Listen("tcp", *schedBind)
	if err != nil {
		log.Fatalf("bind scheduler: %v", err)
	}
	defer schedLis.Close()

	workerLis, err := net.Listen("tcp", *workerBind)
	if err != nil {
		log.Fatalf("bind worker: %v", err)
	}
	defer workerLis.Close()

	schedAddr := schedLis.Addr().String()
	workerAddr := workerLis.Addr().String()
	log.Printf("fcstack: scheduler will bind %s", schedAddr)
	log.Printf("fcstack: worker will bind %s", workerAddr)

	// 4. Build the in-VM agent statically for linux/amd64. The
	//    rootfs pipeline injects this at /.hpcc/agent as PID 1.
	agentPath, err := buildAgent(*stackDir)
	if err != nil {
		log.Fatalf("build agent: %v", err)
	}
	log.Printf("fcstack: built agent at %s", agentPath)

	// 5. Pull the toolchain image and pre-stage its rootfs. The
	//    worker advertises this digest in its Register heartbeat;
	//    the client TOML pins the same digest in remote.image_digest
	//    so scheduler.pickWorker can route on it.
	rootfsDir := filepath.Join(*stackDir, "rootfs")
	pinnedRef, digest, err := prepareRootfs(rootfsDir, *imageRef, agentPath)
	if err != nil {
		log.Fatalf("prepare rootfs: %v", err)
	}
	log.Printf("fcstack: prepared rootfs for %s (digest %s)", pinnedRef, digest)

	// 6. Scheduler — in-process, listens for client Route() and
	//    worker Register/Heartbeat over the same gRPC server.
	schedCfg := scheduler.Config{
		Listen: schedAddr,
		TLS: scheduler.TLSConfig{
			CertFile: certPath,
			KeyFile:  keyPath,
		},
		Auth: scheduler.Auth{
			WorkerToken: workerToken,
			JWKS: scheduler.JWKSAuth{
				URL:      idpURL + "/.well-known/jwks.json",
				Issuer:   idpIssuer,
				Audience: idpAudience,
			},
		},
		Routing: scheduler.Routing{StickyTenants: true},
	}
	sched, err := scheduler.NewScheduler(schedCfg)
	if err != nil {
		log.Fatalf("NewScheduler: %v", err)
	}

	schedCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatalf("load cert: %v", err)
	}
	schedSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{schedCert},
		MinVersion:   tls.VersionTLS13,
	})))
	gen.RegisterSchedulerServiceServer(schedSrv, sched)
	go func() { _ = schedSrv.Serve(schedLis) }()
	defer schedSrv.GracefulStop()

	// 7. Worker — firecracker handler, jailer chroot under
	//    stack-dir/jailer, vm memory + vcpus configurable, idle
	//    timeouts deliberately long so the warm-build run reuses
	//    the same VMs the cold build started.
	runDir := filepath.Join(*stackDir, "jailer")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		log.Fatalf("mkdir runDir: %v", err)
	}
	uid, gid := pickJailerCreds()

	// Paranoid mode: the worker owns the cache; clients never touch a
	// local store. This is both the "regulated environments" pitch
	// hpcc leads with and the only configuration that meaningfully
	// benchmarks the Phase 4 path — in non-paranoid mode every warm
	// compile would short-circuit on the client side and never
	// re-exercise scheduler/worker/FC dispatch, which defeats the
	// purpose of an FC-mode benchmark. The on-disk layout lives at
	// {stack-dir}/cache; the shell wrapper inspects that directory
	// directly to count entries (the client has no `hpcc stats`
	// surface in paranoid mode).
	cacheDir := filepath.Join(*stackDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatalf("mkdir cacheDir: %v", err)
	}

	workerCfg := worker.Config{
		Listen:     workerAddr,
		PublicAddr: workerAddr,
		Paranoid:   true,
		TLS: worker.TLSConfig{
			CertFile: certPath,
			KeyFile:  keyPath,
		},
		Scheduler: worker.SchedulerLink{
			URL:         schedAddr,
			WorkerToken: workerToken,
			CAFile:      certPath,
		},
		Runtime: worker.RuntimeConfig{
			Handler: wruntime.HandlerFirecracker,
			Firecracker: worker.FirecrackerConfig{
				FirecrackerBin: *fcBin,
				JailerBin:      *jailerBin,
				KernelImage:    *kernel,
				RootfsDir:      rootfsDir,
				RunDir:         runDir,
				UID:            uid,
				GID:            gid,
			},
		},
		VM: worker.VMConfig{
			Memory:         *vmMem,
			VCPUs:          int32(*vmVCPUs),
			IdleTimeout:    "1h",
			SessionTimeout: "8h",
		},
		Pool: worker.PoolConfig{MaxActive: *poolMax},
		Image: worker.ImageConfig{
			AdvertisedDigests: []string{digest},
			IdleTimeout:       "24h",
		},
		// Generous max_size so a defconfig kernel build (~30k TUs at
		// a few hundred KB each → low-GB cache) doesn't trip eviction
		// mid-bench and make the warm pass look like cache misses.
		Caches: []config.CacheConfig{{
			Type:     enum.CacheDisk,
			Location: cacheDir,
			MaxSize:  "100G",
		}},
	}

	w, err := worker.NewWorker(workerCfg)
	if err != nil {
		log.Fatalf("NewWorker: %v", err)
	}

	workerCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatalf("load worker cert: %v", err)
	}
	workerSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{workerCert},
		MinVersion:   tls.VersionTLS13,
	})))
	gen.RegisterWorkerServiceServer(workerSrv, w)
	go func() { _ = workerSrv.Serve(workerLis) }()
	defer workerSrv.GracefulStop()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	// 8. Client TOML. Points at our scheduler over TLS (CAFile is
	//    the same self-signed cert we used above), enables remote
	//    dispatch, and supplies OAuth credentials the dispatcher
	//    will exchange for a JWT at session-open time.
	clientToml := fmt.Sprintf(`# fcstack-generated hpcc client config — do not edit by hand
preprocessing_mode = "local"

[remote]
enabled      = true
tenant_id    = %q
image_ref    = %q
image_digest = %q

[remote.scheduler]
url     = %q
ca_file = %q

[remote.oauth]
token_url     = %q
client_id     = "fcstack-bench"
client_secret = "unused"
username      = "bench"
password      = "unused"
scope         = "hpcc"
`, tenantID, pinnedRef, digest, schedAddr, certPath, idpURL+"/token")

	if err := os.WriteFile(*clientCfg, []byte(clientToml), 0o600); err != nil {
		log.Fatalf("write client config: %v", err)
	}

	// Single-line, no-prefix print so the shell wrapper can capture
	// it with `read CONFIG < <(...)` style. Everything else from
	// this binary goes to stderr via log.
	fmt.Println(*clientCfg)
	log.Printf("fcstack: client config at %s", *clientCfg)
	log.Printf("fcstack: ready; waiting for SIGINT/SIGTERM")

	select {
	case err := <-runDone:
		log.Printf("fcstack: worker.Run returned: %v", err)
	case <-ctx.Done():
		log.Printf("fcstack: shutting down")
	}
}

// --- IdP ------------------------------------------------------------------

func startIdP(bind string, key *rsa.PrivateKey, kid string) (string, func(), error) {
	lis, err := net.Listen("tcp", bind)
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		jwk := map[string]any{
			"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
			"n": b64url(key.N.Bytes()),
			"e": b64url(big.NewInt(int64(key.E)).Bytes()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "password" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		claims := jwt.MapClaims{
			"iss": idpIssuer, "aud": idpAudience,
			"sub": r.Form.Get("username"),
			"iat": time.Now().Unix(),
			"nbf": time.Now().Unix(),
			"exp": time.Now().Add(2 * time.Hour).Unix(),
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
			"access_token": signed, "token_type": "Bearer", "expires_in": 7200,
		})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	url := "http://" + lis.Addr().String()
	return url, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// --- cert -----------------------------------------------------------------

func writeSelfSignedCert(dir, name string) (string, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return "", "", err
	}
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

// --- agent + rootfs -------------------------------------------------------

func buildAgent(stackDir string) (string, error) {
	out := filepath.Join(stackDir, "hpcc-agent")
	repoRoot, err := moduleRoot()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, ".")
	cmd.Dir = filepath.Join(repoRoot, "agent")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build agent: %v\n%s", err, b)
	}
	return out, nil
}

// prepareRootfs resolves imageRef to its linux/amd64 manifest digest,
// pins the ref, and runs the rootfs.Store pipeline to write a prepared
// squashfs at rootfsDir/<algo>-<hex>.sqsh with the agent injected.
// Returns the pinned ref (image@sha256:...) and the digest.
func prepareRootfs(rootfsDir, imageRef, agentPath string) (string, string, error) {
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return "", "", err
	}
	if err := os.Chmod(rootfsDir, 0o755); err != nil {
		return "", "", err
	}

	platform := &v1.Platform{OS: "linux", Architecture: "amd64"}
	digest, err := crane.Digest(imageRef, crane.WithPlatform(platform))
	if err != nil {
		return "", "", fmt.Errorf("digest %s: %w", imageRef, err)
	}

	base := imageRef
	if idx := strings.IndexByte(imageRef, '@'); idx >= 0 {
		base = imageRef[:idx]
	} else if idx := strings.LastIndexByte(imageRef, ':'); idx > strings.LastIndexByte(imageRef, '/') {
		base = imageRef[:idx]
	}
	pinnedRef := base + "@" + digest

	store := &rootfs.Store{
		CacheDir: rootfsDir,
		Agent: rootfs.AgentBinaries{
			LinuxAmd64: agentPath,
			LinuxArm64: agentPath,
		},
	}
	if err := store.PullImage(context.Background(), pinnedRef, digest); err != nil {
		return "", "", fmt.Errorf("pull %s: %w", pinnedRef, err)
	}
	return pinnedRef, digest, nil
}

func moduleRoot() (string, error) {
	// Walk up from this file's location looking for go.mod. The bench
	// binary lives under bench/cmd/fcstack/ inside the main module, so
	// runtime.Caller(0) would also work — but resolving via cwd keeps
	// the binary usable when invoked from anywhere.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.work above %s", cwd)
		}
		dir = parent
	}
}

func pickJailerCreds() (int, int) {
	if u := os.Getenv("SUDO_UID"); u != "" {
		if g := os.Getenv("SUDO_GID"); g != "" {
			var ui, gi int
			if _, err := fmt.Sscanf(u, "%d", &ui); err == nil {
				if _, err := fmt.Sscanf(g, "%d", &gi); err == nil && ui > 0 && gi > 0 {
					return ui, gi
				}
			}
		}
	}
	return 65534, 65534
}
