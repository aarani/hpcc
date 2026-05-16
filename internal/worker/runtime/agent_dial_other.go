//go:build !windows

package runtime

import (
	"context"
	"errors"

	"google.golang.org/grpc"
)

// dialContainerAgent is a stub off Windows. The hcsshim runtime only
// ever drives Hyper-V isolation on Windows hosts; this stub keeps
// the package cross-compiling so the Linux build matrix can still
// type-check the worker. NewHcsshim's Isolation validation rejects
// hyperv before any code path reaches this stub at runtime, so the
// error message here is just defensive.
func (h *Hcsshim) dialContainerAgent(_ context.Context, _ string) (*grpc.ClientConn, error) {
	return nil, errors.New("hcsshim: dialContainerAgent only supported on windows")
}
