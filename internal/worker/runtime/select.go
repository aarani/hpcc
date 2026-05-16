package runtime

import "fmt"

// Options bundles backend-specific runtime knobs. Select inspects the
// fields whose handler was chosen and ignores the rest, so callers can
// populate every backend up front and let Select pick.
type Options struct {
	Firecracker FirecrackerOptions
	Hcsshim     HcsshimOptions
}

// Select returns the Runtime implementation that matches the configured
// runtime.handler value. Unimplemented backends return a clear error
// at worker startup rather than panicking later on the first Compile.
//
// Recognized values:
//
//	"really_really_dangerous"  — DangerouslyExecOnHost; dev only.
//	"firecracker"              — raw Firecracker driver (Linux).
//	"runhcs-wcow-hypervisor"   — containerd + hcsshim Hyper-V isolation
//	                             (Windows).
func Select(handler string, opts Options) (Runtime, error) {
	switch handler {
	case HandlerReallyReallyDangerous:
		return DangerouslyExecOnHost{}, nil
	case HandlerFirecracker:
		return NewFirecracker(opts.Firecracker)
	case HandlerHcsshim:
		return NewHcsshim(opts.Hcsshim)
	default:
		return nil, fmt.Errorf("unknown runtime.handler %q", handler)
	}
}
