//go:build !windows

package runtime

// grantContainerReadExecute is a no-op off Windows. The hcsshim
// runtime only ever runs on Windows in production; this stub exists so
// the rest of the package cross-compiles cleanly on the Linux build
// matrix, where ACLs aren't a meaningful concept for the staged pause
// binary (filesystem perms are set explicitly by stagePauseMount).
func grantContainerReadExecute(_ string) error { return nil }
func grantContainerModify(_ string) error      { return nil }
