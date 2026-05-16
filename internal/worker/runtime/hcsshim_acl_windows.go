//go:build windows

package runtime

import (
	"fmt"
	"os/exec"
)

// grantContainerReadExecute relaxes the host ACLs on the pause-mount
// directory so the in-container user (typically the well-known
// ContainerUser SID in nanoserver/servercore) can read and execute
// the bind-mounted pause binary.
//
// Without this, the staged file inherits the GitHub-Actions admin
// runner's restrictive temp-dir ACL: ContainerUser has no entry, and
// hcs::System::CreateProcess fails with ERROR_ACCESS_DENIED when the
// runhcs shim tries to launch C:\.hpcc\pause.exe inside the silo /
// utility VM.
//
// We grant *S-1-1-0 (Everyone) RX rather than the ContainerUser SID
// because Everyone covers both the process-isolation case (where
// ContainerUser is a real local SID) and Hyper-V isolation (where the
// VSMB share is mapped under a different identity). The binary is
// pause.exe — read+execute on the host bind-mount root has no
// security consequence above what the kernel boundary already gives.
//
// (OI)(CI) makes the grant inheritable so the file added by
// stagePauseMount picks it up; /T applies to anything already there.
func grantContainerReadExecute(path string) error {
	cmd := exec.Command("icacls", path, "/grant", "*S-1-1-0:(OI)(CI)RX", "/T")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %w (%s)", path, err, out)
	}
	return nil
}

// grantContainerModify is the read/write counterpart used for the per-
// container src/out scratch dirs. The compile needs to read source
// staged from the host (src) and write outputs back (out); both surface
// through bind mounts that inherit the host ACL. Modify is enough —
// the container shouldn't need to take ownership or change ACLs.
// (OI)(CI) propagates to per-Exec subdirectories created later.
func grantContainerModify(path string) error {
	cmd := exec.Command("icacls", path, "/grant", "*S-1-1-0:(OI)(CI)M", "/T")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %w (%s)", path, err, out)
	}
	return nil
}
