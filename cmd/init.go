/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/aarani/hpcc/internal/bootstrap"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/aarani/hpcc/internal/worker"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Bootstrap config files for distributed components",
		Long: `Generate a working scheduler, worker, or client config so an
operator doesn't have to hand-edit TOML on first install.

Each subcommand writes ~/.config/hpcc/{scheduler,worker,config}.toml
with sane defaults plus the bits that can't be defaulted (TLS material,
scheduler URL, tenant IdP, image digest). See
docs/{scheduler,worker,client}.toml for the full annotated reference of
every knob.`,
	}
}

// --- scheduler -------------------------------------------------------

func newInitSchedulerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scheduler",
		Short: "Write scheduler.toml with a generated worker_token",
		Long: `Write scheduler.toml at ~/.config/hpcc/scheduler.toml (or
--config <path>). A 32-byte random worker_token is generated and
embedded; the same token must be passed to ` + "`hpcc init worker --token`" + `
on every worker.

Clients dial the scheduler over TLS and validate the chain, so the
scheduler needs a real certificate trusted by the clients' system trust
store — either a public CA or your org's internal CA. Pass --cert-file/
--key-file pointing at that material, or --cert-ref/--key-ref to load
it from a secret store (aws-sm://, env:, file:).

At least one tenant is required (see docs/plan/multi-tenant.md). Pass
the tenant flags below; additional tenants can be appended by hand-
editing the file.`,
		RunE: runInitScheduler,
	}
	cmd.Flags().String("config", "", "path to scheduler.toml (default: $XDG_CONFIG_HOME/hpcc/scheduler.toml)")
	cmd.Flags().Bool("force", false, "overwrite existing config")
	cmd.Flags().String("listen", ":9091", "gRPC listen address")
	cmd.Flags().String("metrics-listen", ":9191", "Prometheus /metrics listen address (empty disables)")
	cmd.Flags().String("cert-file", "", "path to TLS cert (PEM) trusted by clients (e.g. issued by your org CA)")
	cmd.Flags().String("key-file", "", "path to TLS private key (PEM)")
	cmd.Flags().String("cert-ref", "", "secret reference for TLS cert (aws-sm://, env:, file:)")
	cmd.Flags().String("key-ref", "", "secret reference for TLS key")
	cmd.Flags().String("tenant-id", "", "tenant ID (required)")
	cmd.Flags().String("issuer", "", "tenant IdP issuer URL (required)")
	cmd.Flags().String("jwks-url", "", "tenant IdP JWKS URL (required)")
	cmd.Flags().String("token-url", "", "tenant IdP token URL (required)")
	cmd.Flags().String("audience", "", "tenant IdP audience (required)")
	cmd.Flags().String("client-id", "", "OAuth client_id served back to clients via GetTenantIdP")
	cmd.Flags().String("scope", "", "OAuth scope served back to clients via GetTenantIdP")
	cmd.Flags().Bool("sticky-tenants", true, "bias routing so (tenant, image) pairs land on the worker with a warm VM")
	cmd.Flags().Bool("paranoid", false, "deployment-wide paranoid mode (must match worker)")
	return cmd
}

func runInitScheduler(cmd *cobra.Command, _ []string) error {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	if path == "" {
		p, err := scheduler.DefaultConfigPath()
		if err != nil {
			return err
		}
		path = p
	}
	force, _ := cmd.Flags().GetBool("force")

	certFile, _ := cmd.Flags().GetString("cert-file")
	keyFile, _ := cmd.Flags().GetString("key-file")
	certRef, _ := cmd.Flags().GetString("cert-ref")
	keyRef, _ := cmd.Flags().GetString("key-ref")
	if (certFile == "") == (certRef == "") {
		return fmt.Errorf("exactly one of --cert-file or --cert-ref is required")
	}
	if (keyFile == "") == (keyRef == "") {
		return fmt.Errorf("exactly one of --key-file or --key-ref is required")
	}

	tenantID, _ := cmd.Flags().GetString("tenant-id")
	issuer, _ := cmd.Flags().GetString("issuer")
	jwksURL, _ := cmd.Flags().GetString("jwks-url")
	tokenURL, _ := cmd.Flags().GetString("token-url")
	audience, _ := cmd.Flags().GetString("audience")
	clientID, _ := cmd.Flags().GetString("client-id")
	scope, _ := cmd.Flags().GetString("scope")
	for name, v := range map[string]string{
		"--tenant-id": tenantID,
		"--issuer":    issuer,
		"--jwks-url":  jwksURL,
		"--token-url": tokenURL,
		"--audience":  audience,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	listen, _ := cmd.Flags().GetString("listen")
	metricsListen, _ := cmd.Flags().GetString("metrics-listen")
	sticky, _ := cmd.Flags().GetBool("sticky-tenants")
	paranoid, _ := cmd.Flags().GetBool("paranoid")

	token, err := bootstrap.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate worker token: %w", err)
	}

	data := schedulerTemplateData{
		Listen:        listen,
		MetricsListen: metricsListen,
		CertFile:      certFile,
		KeyFile:       keyFile,
		CertRef:       certRef,
		KeyRef:        keyRef,
		WorkerToken:   token,
		TenantID:      tenantID,
		Issuer:        issuer,
		JWKSURL:       jwksURL,
		TokenURL:      tokenURL,
		Audience:      audience,
		ClientID:      clientID,
		Scope:         scope,
		StickyTenants: sticky,
		Paranoid:      paranoid,
	}

	var buf bytes.Buffer
	if err := schedulerTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render config: %w", err)
	}

	if err := writeConfigFile(path, buf.Bytes(), 0o600, force); err != nil {
		return err
	}

	if err := validateSchedulerOutput(path); err != nil {
		return fmt.Errorf("generated config failed validation: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote %s\n", path)
	fmt.Fprintf(out, "\nworker_token: %s\n", token)
	fmt.Fprintf(out, "\nOn each worker host, run:\n")
	fmt.Fprintf(out, "  hpcc init worker --scheduler <scheduler-host:%s> --token %s --public-addr <this-host:9092>\n",
		portFromListen(listen), token)
	fmt.Fprintf(out, "\nStart the scheduler with:\n  hpcc scheduler\n")
	return nil
}

// --- worker ----------------------------------------------------------

func newInitWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Write worker.toml with self-signed TLS material",
		Long: `Write worker.toml at ~/.config/hpcc/worker.toml (or --config
<path>). A self-signed TLS leaf is minted next to the config (worker.crt
+ worker.key); the scheduler records its SHA-256 fingerprint at
registration and clients pin against that fingerprint on dial, so a
self-signed cert is sufficient.

--scheduler and --token come from the corresponding ` + "`hpcc init scheduler`" + `
output. --public-addr is the address clients will dial after the
scheduler routes them — it must be reachable from wherever clients run.`,
		RunE: runInitWorker,
	}
	cmd.Flags().String("config", "", "path to worker.toml (default: $XDG_CONFIG_HOME/hpcc/worker.toml)")
	cmd.Flags().Bool("force", false, "overwrite existing config and TLS material")
	cmd.Flags().String("scheduler", "", "scheduler URL host:port (required)")
	cmd.Flags().String("token", "", "worker_token from `hpcc init scheduler` output (required)")
	cmd.Flags().String("public-addr", "", "address clients dial after scheduler routing, host:port (required)")
	cmd.Flags().String("listen", ":9092", "gRPC listen address")
	cmd.Flags().String("metrics-listen", ":9192", "Prometheus /metrics listen address (empty disables)")
	cmd.Flags().String("runtime", "firecracker", "runtime handler: firecracker | runhcs-wcow-hypervisor | really_really_dangerous")
	cmd.Flags().String("scheduler-ca-file", "", "optional: pin the scheduler's CA cert (PEM)")
	return cmd
}

func runInitWorker(cmd *cobra.Command, _ []string) error {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	if path == "" {
		p, err := worker.DefaultConfigPath()
		if err != nil {
			return err
		}
		path = p
	}
	force, _ := cmd.Flags().GetBool("force")

	schedulerURL, _ := cmd.Flags().GetString("scheduler")
	token, _ := cmd.Flags().GetString("token")
	publicAddr, _ := cmd.Flags().GetString("public-addr")
	for name, v := range map[string]string{
		"--scheduler":   schedulerURL,
		"--token":       token,
		"--public-addr": publicAddr,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	listen, _ := cmd.Flags().GetString("listen")
	metricsListen, _ := cmd.Flags().GetString("metrics-listen")
	runtimeHandler, _ := cmd.Flags().GetString("runtime")
	caFile, _ := cmd.Flags().GetString("scheduler-ca-file")

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}
	certPath := filepath.Join(dir, "worker.crt")
	keyPath := filepath.Join(dir, "worker.key")

	if !force {
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists; pass --force to overwrite", p)
			}
		}
	}

	hosts := []string{hostFromAddr(publicAddr)}
	certPEM, keyPEM, err := bootstrap.GenerateSelfSignedTLS(bootstrap.SelfSignedOptions{
		CommonName: hosts[0],
		Hosts:      hosts,
	})
	if err != nil {
		return fmt.Errorf("generate self-signed TLS: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	data := workerTemplateData{
		Listen:         listen,
		MetricsListen:  metricsListen,
		PublicAddr:     publicAddr,
		CertFile:       certPath,
		KeyFile:        keyPath,
		SchedulerURL:   schedulerURL,
		WorkerToken:    token,
		CAFile:         caFile,
		RuntimeHandler: runtimeHandler,
	}

	var buf bytes.Buffer
	if err := workerTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	if err := writeConfigFile(path, buf.Bytes(), 0o600, force); err != nil {
		return err
	}
	if err := validateWorkerOutput(path); err != nil {
		return fmt.Errorf("generated config failed validation: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote %s\n", path)
	fmt.Fprintf(out, "wrote %s\n", certPath)
	fmt.Fprintf(out, "wrote %s\n", keyPath)
	switch runtimeHandler {
	case "firecracker":
		fmt.Fprintf(out, "\nfirecracker runtime selected. Before `hpcc worker` starts, the host needs:\n")
		fmt.Fprintf(out, "  /usr/bin/firecracker and /usr/bin/jailer    (no distro package; download\n")
		fmt.Fprintf(out, "                                               the upstream tarball — see\n")
		fmt.Fprintf(out, "                                               README's worker section)\n")
		fmt.Fprintf(out, "  /var/lib/hpcc/vmlinux                       (kernel image)\n")
		fmt.Fprintf(out, "  /var/lib/hpcc/hpcc-agent-linux-amd64        (agent binary)\n")
		fmt.Fprintf(out, "If your layout differs, edit [runtime.firecracker] / [image] in %s.\n", path)
	case "runhcs-wcow-hypervisor":
		fmt.Fprintf(out, "\nrunhcs (Windows Hyper-V) runtime selected. Before `hpcc worker` starts:\n")
		fmt.Fprintf(out, "  containerd running on \\\\.\\pipe\\containerd-containerd\n")
		fmt.Fprintf(out, "  C:\\ProgramData\\hpcc\\hpcc-agent.exe                  (agent binary)\n")
		fmt.Fprintf(out, "  Hyper-V Windows feature installed, vmcompute service running\n")
		fmt.Fprintf(out, "If your layout differs, edit [runtime.hcsshim] / [image] in %s.\n", path)
	case "really_really_dangerous":
		fmt.Fprintf(out, "\ndev-mode runtime selected — compiles run as worker child processes with\n")
		fmt.Fprintf(out, "no isolation. Never use outside a throwaway environment.\n")
	}
	fmt.Fprintf(out, "\nStart the worker with:\n  hpcc worker\n")
	return nil
}

// --- client ----------------------------------------------------------

func newInitClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Write config.toml pointed at a scheduler",
		Long: `Write config.toml at ~/.config/hpcc/config.toml (or --config
<path>). The generated file enables [remote] dispatch against the
given scheduler/tenant/image and configures one local disk cache.

OAuth credentials are NOT written here — after init, run
` + "`hpcc auth login`" + ` to cache an access token at token.json. The
daemon refreshes it silently.

--image-digest is the SHA-256 the worker will pull the toolchain image
under. --image-ref is the optional human-readable name (e.g.
ghcr.io/example/toolchain@sha256:...). Both come from your image
registry; the digest is the source of truth, the ref is for humans.`,
		RunE: runInitClient,
	}
	cmd.Flags().String("config", "", "path to config.toml (default: $XDG_CONFIG_HOME/hpcc/config.toml)")
	cmd.Flags().Bool("force", false, "overwrite existing config")
	cmd.Flags().String("scheduler", "", "scheduler URL host:port (required)")
	cmd.Flags().String("tenant", "", "tenant ID, must match a [[tenant]] on the scheduler (required)")
	cmd.Flags().String("image-digest", "", "toolchain image digest, e.g. sha256:abc... (required)")
	cmd.Flags().String("image-ref", "", "optional human-readable image reference")
	cmd.Flags().String("scheduler-ca-file", "", "optional: pin the scheduler's CA cert (PEM); empty = system trust store")
	cmd.Flags().String("source-mode", "cas", "cache-key derivation: cas | preprocessed")
	cmd.Flags().String("cache-dir", "/tmp/hpcc", "local disk cache directory")
	cmd.Flags().String("cache-size", "10G", "local disk cache size limit")
	return cmd
}

func runInitClient(cmd *cobra.Command, _ []string) error {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	if path == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			return err
		}
		path = p
	}
	force, _ := cmd.Flags().GetBool("force")

	schedulerURL, _ := cmd.Flags().GetString("scheduler")
	tenant, _ := cmd.Flags().GetString("tenant")
	imageDigest, _ := cmd.Flags().GetString("image-digest")
	for name, v := range map[string]string{
		"--scheduler":    schedulerURL,
		"--tenant":       tenant,
		"--image-digest": imageDigest,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	imageRef, _ := cmd.Flags().GetString("image-ref")
	caFile, _ := cmd.Flags().GetString("scheduler-ca-file")
	sourceMode, _ := cmd.Flags().GetString("source-mode")
	cacheDir, _ := cmd.Flags().GetString("cache-dir")
	cacheSize, _ := cmd.Flags().GetString("cache-size")

	data := clientTemplateData{
		SourceMode:   sourceMode,
		CacheDir:     cacheDir,
		CacheSize:    cacheSize,
		Tenant:       tenant,
		ImageRef:     imageRef,
		ImageDigest:  imageDigest,
		SchedulerURL: schedulerURL,
		CAFile:       caFile,
	}
	var buf bytes.Buffer
	if err := clientTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	if err := writeConfigFile(path, buf.Bytes(), 0o600, force); err != nil {
		return err
	}
	if _, err := config.LoadConfig(path); err != nil {
		return fmt.Errorf("generated config failed to parse: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote %s\n", path)
	fmt.Fprintf(out, "\nNext: cache an OAuth token, then start the daemon:\n")
	fmt.Fprintf(out, "  hpcc auth login\n")
	fmt.Fprintf(out, "  hpcc start\n")
	return nil
}

// --- helpers ---------------------------------------------------------

type schedulerTemplateData struct {
	Listen        string
	MetricsListen string
	CertFile      string
	KeyFile       string
	CertRef       string
	KeyRef        string
	WorkerToken   string
	TenantID      string
	Issuer        string
	JWKSURL       string
	TokenURL      string
	Audience      string
	ClientID      string
	Scope         string
	StickyTenants bool
	Paranoid      bool
}

type clientTemplateData struct {
	SourceMode   string
	CacheDir     string
	CacheSize    string
	Tenant       string
	ImageRef     string
	ImageDigest  string
	SchedulerURL string
	CAFile       string
}

type workerTemplateData struct {
	Listen         string
	MetricsListen  string
	PublicAddr     string
	CertFile       string
	KeyFile        string
	SchedulerURL   string
	WorkerToken    string
	CAFile         string
	RuntimeHandler string
}

// Templates are intentionally minimal — every field has a one-liner
// pointing readers at docs/{scheduler,worker}.toml for the full
// annotated reference. Keeping the generated file small means an
// operator can scan it in one screen.

var schedulerTemplate = template.Must(template.New("scheduler").Parse(
	`# hpcc scheduler config — generated by ` + "`hpcc init scheduler`" + `.
# See docs/scheduler.toml for the full annotated reference.

listen = "{{.Listen}}"
{{if .MetricsListen}}metrics_listen = "{{.MetricsListen}}"
{{end}}{{if .Paranoid}}paranoid = true
{{end}}
[tls]
{{if .CertFile}}cert_file = {{printf "%q" .CertFile}}
{{else}}cert_ref = {{printf "%q" .CertRef}}
{{end}}{{if .KeyFile}}key_file  = {{printf "%q" .KeyFile}}
{{else}}key_ref  = {{printf "%q" .KeyRef}}
{{end}}
[auth]
worker_token = {{printf "%q" .WorkerToken}}

[[tenant]]
id        = {{printf "%q" .TenantID}}
issuer    = {{printf "%q" .Issuer}}
jwks_url  = {{printf "%q" .JWKSURL}}
token_url = {{printf "%q" .TokenURL}}
audience  = {{printf "%q" .Audience}}
{{if .ClientID}}client_id = {{printf "%q" .ClientID}}
{{end}}{{if .Scope}}scope     = {{printf "%q" .Scope}}
{{end}}
[routing]
sticky_tenants = {{.StickyTenants}}
`))

var workerTemplate = template.Must(template.New("worker").Parse(
	`# hpcc worker config — generated by ` + "`hpcc init worker`" + `.
# See docs/worker.toml for the full annotated reference.

listen      = "{{.Listen}}"
{{if .MetricsListen}}metrics_listen = "{{.MetricsListen}}"
{{end}}public_addr = {{printf "%q" .PublicAddr}}

[tls]
cert_file = {{printf "%q" .CertFile}}
key_file  = {{printf "%q" .KeyFile}}

[scheduler]
url          = {{printf "%q" .SchedulerURL}}
worker_token = {{printf "%q" .WorkerToken}}
{{if .CAFile}}ca_file      = {{printf "%q" .CAFile}}
{{end}}
[runtime]
handler = {{printf "%q" .RuntimeHandler}}
{{if eq .RuntimeHandler "firecracker"}}
# Standard host paths; stage the binaries here (or edit to match where
# you have them) before starting the worker.
[runtime.firecracker]
firecracker_bin = "/usr/bin/firecracker"
jailer_bin      = "/usr/bin/jailer"
kernel_image    = "/var/lib/hpcc/vmlinux"
rootfs_dir      = "/var/lib/hpcc/rootfs"
run_dir         = "/srv/jailer"
uid             = 1000
gid             = 1000

[image]
agent_linux_amd64 = "/var/lib/hpcc/hpcc-agent-linux-amd64"
# agent_linux_arm64 = "/var/lib/hpcc/hpcc-agent-linux-arm64"
{{end}}{{if eq .RuntimeHandler "runhcs-wcow-hypervisor"}}
# Standard Windows paths; stage hpcc-agent.exe at the agent path and
# make sure containerd is running on the named pipe before starting the
# worker. Hyper-V isolation is the production value; "process" loses
# the kernel boundary and is only for hosts without nested virt.
[runtime.hcsshim]
address     = "\\\\.\\pipe\\containerd-containerd"
namespace   = "hpcc"
run_dir     = "C:\\ProgramData\\hpcc\\run"
runtime     = "io.containerd.runhcs.v1"
snapshotter = "windows"
isolation   = "hyperv"

[image]
agent_windows_amd64 = "C:\\ProgramData\\hpcc\\hpcc-agent.exe"
{{end}}
[vm]
memory          = "2GB"
vcpus           = 4
idle_timeout    = "10m"
session_timeout = "8h"

[pool]
max_active = 32
`))

var clientTemplate = template.Must(template.New("client").Parse(
	`# hpcc client config — generated by ` + "`hpcc init client`" + `.
# See docs/client.toml for the full annotated reference.

source_mode = {{printf "%q" .SourceMode}}

[[cache]]
type     = "disk"
location = {{printf "%q" .CacheDir}}
max_size = {{printf "%q" .CacheSize}}

[remote]
enabled      = true
tenant_id    = {{printf "%q" .Tenant}}
{{if .ImageRef}}image_ref    = {{printf "%q" .ImageRef}}
{{end}}image_digest = {{printf "%q" .ImageDigest}}

[remote.scheduler]
url     = {{printf "%q" .SchedulerURL}}
{{if .CAFile}}ca_file = {{printf "%q" .CAFile}}
{{end}}`))

func writeConfigFile(path string, body []byte, mode os.FileMode, force bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat %q: %w", path, err)
		}
	}
	return writeFileAtomic(path, body, mode)
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hpcc-init-*")
	if err != nil {
		return fmt.Errorf("create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %q -> %q: %w", tmpName, path, err)
	}
	return nil
}

func validateSchedulerOutput(path string) error {
	cfg, err := scheduler.LoadConfig(path)
	if err != nil {
		return err
	}
	return cfg.Validate()
}

func validateWorkerOutput(path string) error {
	cfg, err := worker.LoadConfig(path)
	if err != nil {
		return err
	}
	return cfg.Validate()
}

// portFromListen extracts the port from a ":NNNN" or "host:NNNN"
// listen address. Best-effort — used only to build the help text we
// print after `hpcc init scheduler`.
func portFromListen(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 && i+1 < len(listen) {
		return listen[i+1:]
	}
	return "9091"
}

// hostFromAddr returns the host portion of a "host:port" string, or
// the input unchanged if no port is present. Used to populate the
// self-signed cert's SAN.
func hostFromAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func init() {
	root := newInitCmd()
	root.AddCommand(newInitSchedulerCmd())
	root.AddCommand(newInitWorkerCmd())
	root.AddCommand(newInitClientCmd())
	rootCmd.AddCommand(root)
}
