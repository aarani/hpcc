package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aarani/hpcc/internal/config"
)

type Config struct {
	Listen     string        `toml:"listen"`      // gRPC listen addr for incoming Compile RPCs
	WorkerID   string        `toml:"worker_id"`   // empty → auto-generate at startup
	PublicAddr string        `toml:"public_addr"` // address advertised to the scheduler; clients dial this
	Paranoid   bool          `toml:"paranoid"`    // mirror of scheduler-side paranoid mode (§4.13)
	TLS        TLSConfig     `toml:"tls"`
	Scheduler  SchedulerLink `toml:"scheduler"`
	Runtime    RuntimeConfig `toml:"runtime"`
	VM         VMConfig      `toml:"vm"`
	Pool       PoolConfig    `toml:"pool"`
	Image      ImageConfig   `toml:"image"`

	// Caches is the worker-side cache backends. In paranoid mode (§4.13)
	// the worker is the only process that reads/writes the cache; in the
	// default mode this can be empty and clients carry their own caches.
	// Schema is shared with the client config (config.CacheConfig).
	Caches []config.CacheConfig `toml:"cache"`
}

type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

type SchedulerLink struct {
	URL         string `toml:"url"`          // e.g. "scheduler.internal:9091"
	WorkerToken string `toml:"worker_token"` // shared static token; matches scheduler.auth.worker_token
	CAFile      string `toml:"ca_file"`      // optional: pin the scheduler's CA cert
}

type RuntimeConfig struct {
	// Handler selects the worker runtime backend.
	//   "firecracker"             — raw Firecracker driver (Linux).
	//   "runhcs-wcow-hypervisor"  — containerd + hcsshim Hyper-V (Windows).
	//   "really_really_dangerous" — host-exec; dev only.
	Handler string `toml:"handler"`

	Firecracker FirecrackerConfig `toml:"firecracker"`
	Hcsshim     HcsshimConfig     `toml:"hcsshim"`
}

// FirecrackerConfig is the raw-Firecracker runtime's host-side knobs.
// Unused unless runtime.handler == "firecracker".
//
// FirecrackerBin and JailerBin are the host paths to the upstream
// firecracker and jailer executables. KernelImage is the vmlinux that
// every microVM boots — hpcc owns the kernel, not the user image
// (plan §4.3). RootfsDir mirrors the rootfs.Store CacheDir so the
// runtime can resolve a prepared "<algo>-<hex>.ext4" by digest.
// RunDir is jailer's --chroot-base-dir; one chroot per VM lands
// underneath as <RunDir>/firecracker/<vm-id>/root/. UID/GID are the
// non-root credentials jailer drops to before exec'ing firecracker.
// BootArgs overrides the default kernel cmdline (sane defaults boot
// the rootfs read-only with the in-VM hpcc-agent as PID 1).
type FirecrackerConfig struct {
	FirecrackerBin string `toml:"firecracker_bin"`
	JailerBin      string `toml:"jailer_bin"`
	KernelImage    string `toml:"kernel_image"`
	RootfsDir      string `toml:"rootfs_dir"`
	RunDir         string `toml:"run_dir"`
	UID            int    `toml:"uid"`
	GID            int    `toml:"gid"`
	BootArgs       string `toml:"boot_args"`
}

// HcsshimConfig is the containerd + hcsshim runtime's host-side knobs.
// Unused unless runtime.handler == "runhcs-wcow-hypervisor".
//
// Address is the containerd UDS / named-pipe address (Windows default:
// "\\\\.\\pipe\\containerd-containerd"). Namespace is the containerd
// namespace hpcc keeps its prepared images and containers under;
// isolation between hpcc state and anything else on the host is
// containerd-namespace-level. RunDir is hpcc's per-container scratch
// root — each Start allocates <RunDir>/<container-id>/{src,out} as the
// host backing for the per-Exec mounts the runtime injects at C:\src
// and C:\out (§4.1.1, "stage source onto a local volume the container
// mounts"). Runtime overrides the OCI runtime name containerd selects;
// the default ("io.containerd.runhcs.v1") matches the Hyper-V-isolated
// Windows containers shim. Snapshotter selects the containerd
// snapshotter that materializes the prepared image's rootfs; the
// default ("windows") is the standard WCOW snapshotter.
//
// Isolation selects the container isolation mode the runhcs shim
// applies. The production value is "hyperv" — each container is a
// separate Hyper-V utility VM, which is the kernel boundary the
// regulated-enterprise story (§4.1) depends on. "process" runs the
// container in a Windows Server silo on the host kernel; it loses the
// security boundary and is only valid for environments that cannot
// nest virtualization (GitHub Actions hosted runners, dev laptops
// without Hyper-V, etc.). Empty falls back to "hyperv".
type HcsshimConfig struct {
	Address     string `toml:"address"`
	Namespace   string `toml:"namespace"`
	RunDir      string `toml:"run_dir"`
	Runtime     string `toml:"runtime"`
	Snapshotter string `toml:"snapshotter"`
	Isolation   string `toml:"isolation"`
}

type VMConfig struct {
	Memory         string `toml:"memory"` // e.g. "2GB"
	VCPUs          int32  `toml:"vcpus"`
	IdleTimeout    string `toml:"idle_timeout"`    // e.g. "10m"
	SessionTimeout string `toml:"session_timeout"` // e.g. "8h"
}

type PoolConfig struct {
	MaxActive int `toml:"max_active"` // upper bound on concurrent per-tenant VMs
}

// ImageConfig points at pre-built pause binaries on disk. Empty paths
// fall back to the binaries embedded in the worker (built from /pause).
//
// AdvertisedDigests is a static list of image digests the worker
// reports in RegisterWorker / Heartbeat in addition to whatever the
// real ImageStore knows about. Useful when the runtime is the
// dev-mode "really_really_dangerous" handler (no containerd, no
// image store) or when an operator wants to manually pin which
// toolchains this worker accepts. AdvertisedDigests are exempt from
// idle-image eviction.
//
// IdleTimeout is the maximum time an image entry may sit in the local
// catalogue without being used by a Compile before the eviction loop
// drops it (untagging the prepared image in the backing image store
// — containerd on Windows, hpcc's rootfs cache on Linux — and freeing
// its blobs at the next GC). Empty disables eviction.
type ImageConfig struct {
	PauseLinuxAmd64   string   `toml:"pause_linux_amd64"`
	PauseLinuxArm64   string   `toml:"pause_linux_arm64"`
	PauseWindowsAmd64 string   `toml:"pause_windows_amd64"`
	AdvertisedDigests []string `toml:"advertised_digests"`
	IdleTimeout       string   `toml:"idle_timeout"` // e.g. "24h"; empty disables eviction
}

// IdleTimeoutDur parses Image.IdleTimeout. An empty string returns 0
// (eviction disabled).
func (i ImageConfig) IdleTimeoutDur() (time.Duration, error) {
	if i.IdleTimeout == "" {
		return 0, nil
	}
	return time.ParseDuration(i.IdleTimeout)
}

func DefaultConfig() Config {
	return Config{
		Listen: ":9092",
		Runtime: RuntimeConfig{
			Handler: "firecracker",
		},
		VM: VMConfig{
			Memory:         "2GB",
			VCPUs:          4,
			IdleTimeout:    "10m",
			SessionTimeout: "8h",
		},
		Pool: PoolConfig{
			MaxActive: 32,
		},
		Image: ImageConfig{
			IdleTimeout: "24h",
		},
	}
}

// DefaultConfigPath returns ~/.config/hpcc/worker.toml on Unix and the
// platform equivalent elsewhere.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "hpcc", "worker.toml"), nil
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("stat config %q: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.PublicAddr == "" {
		return fmt.Errorf("public_addr is required (clients dial it after scheduler routing)")
	}
	if c.TLS.CertFile == "" {
		return fmt.Errorf("tls.cert_file is required")
	}
	if c.TLS.KeyFile == "" {
		return fmt.Errorf("tls.key_file is required")
	}
	if c.Scheduler.URL == "" {
		return fmt.Errorf("scheduler.url is required")
	}
	if c.Scheduler.WorkerToken == "" {
		return fmt.Errorf("scheduler.worker_token is required")
	}
	if len(c.Scheduler.WorkerToken) < 16 {
		return fmt.Errorf("scheduler.worker_token must be at least 16 characters")
	}
	if c.Runtime.Handler == "" {
		return fmt.Errorf("runtime.handler is required")
	}
	if c.VM.VCPUs <= 0 {
		return fmt.Errorf("vm.vcpus must be > 0")
	}
	if _, err := config.ParseSize(c.VM.Memory); err != nil {
		return fmt.Errorf("vm.memory: %w", err)
	}
	if _, err := c.VM.IdleTimeoutDur(); err != nil {
		return fmt.Errorf("vm.idle_timeout: %w", err)
	}
	if _, err := c.VM.SessionTimeoutDur(); err != nil {
		return fmt.Errorf("vm.session_timeout: %w", err)
	}
	if _, err := c.Image.IdleTimeoutDur(); err != nil {
		return fmt.Errorf("image.idle_timeout: %w", err)
	}
	if c.Pool.MaxActive <= 0 {
		return fmt.Errorf("pool.max_active must be > 0")
	}
	if c.Paranoid && len(c.Caches) == 0 {
		return fmt.Errorf("paranoid mode requires at least one [[cache]] block (clients can't reach a cache)")
	}
	return nil
}

func (v VMConfig) MemoryBytes() int64 {
	n, err := config.ParseSize(v.Memory)
	if err != nil {
		panic(err)
	}
	if n <= 0 {
		panic(fmt.Errorf("must be > 0"))
	}
	return n
}

func (v VMConfig) IdleTimeoutDur() (time.Duration, error) {
	return time.ParseDuration(v.IdleTimeout)
}

func (v VMConfig) SessionTimeoutDur() (time.Duration, error) {
	return time.ParseDuration(v.SessionTimeout)
}
