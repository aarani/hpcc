package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDangerouslyExecOnHost_RunsCommandAndCapturesStdout(t *testing.T) {
	rt := DangerouslyExecOnHost{}
	ctx := context.Background()

	c, err := rt.Start(ctx, ContainerSpec{ID: "c1", TenantID: "t1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(ctx) })

	var stdout, stderr bytes.Buffer
	res, err := c.Exec(ctx, ExecRequest{
		ExecID: "e1",
		Argv:   []string{"/bin/echo", "hello"},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr=%q)", res.ExitCode, stderr.String())
	}
	if got := stdout.String(); got != "hello\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\n")
	}
}

func TestDangerouslyExecOnHost_TranslatesSrcAndOutPaths(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "marker.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	rt := DangerouslyExecOnHost{}
	ctx := context.Background()
	c, err := rt.Start(ctx, ContainerSpec{
		ID:       "c2",
		TenantID: "t2",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(ctx) })

	// Use cp to read /src/marker.txt and write to /out/copied.txt — both
	// in-container paths must translate to the host tmpdirs for this to
	// succeed.
	var stderr bytes.Buffer
	res, err := c.Exec(ctx, ExecRequest{
		ExecID:      "e2",
		Argv:        []string{"/bin/cp", "/src/marker.txt", "/out/copied.txt"},
		SrcHostPath: srcDir,
		OutHostPath: outDir,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (stderr=%q)", res.ExitCode, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(outDir, "copied.txt"))
	if err != nil {
		t.Fatalf("read copied output: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("copied = %q, want %q", got, "ok")
	}
}

func TestDangerouslyExecOnHost_NonZeroExitIsData(t *testing.T) {
	rt := DangerouslyExecOnHost{}
	ctx := context.Background()
	c, _ := rt.Start(ctx, ContainerSpec{ID: "c3"})
	t.Cleanup(func() { _ = c.Stop(ctx) })

	res, err := c.Exec(ctx, ExecRequest{
		ExecID: "e3",
		Argv:   []string{"/bin/sh", "-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("Exec returned err for non-zero exit: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestDangerouslyExecOnHost_DispatchErrorIsErr(t *testing.T) {
	rt := DangerouslyExecOnHost{}
	ctx := context.Background()
	c, _ := rt.Start(ctx, ContainerSpec{ID: "c4"})
	t.Cleanup(func() { _ = c.Stop(ctx) })

	_, err := c.Exec(ctx, ExecRequest{
		ExecID: "e4",
		Argv:   []string{"/no/such/binary/anywhere"},
	})
	if err == nil {
		t.Fatal("expected dispatch error for missing binary, got nil")
	}
}

func TestRewriteRoot_BoundaryAware(t *testing.T) {
	cases := []struct {
		in, root, repl, want string
	}{
		{"/src/foo.cpp", "/src", "/tmp/staged", "/tmp/staged/foo.cpp"},
		{"-I/src/include", "/src", "/tmp/staged", "-I/tmp/staged/include"},
		{"/src", "/src", "/tmp/staged", "/tmp/staged"},
		{"/src-other/x", "/src", "/tmp/staged", "/src-other/x"},   // sibling, not rewritten
		{"-DFOO=/src/x", "/src", "/tmp/staged", "-DFOO=/tmp/staged/x"},
	}
	for _, c := range cases {
		got := rewriteRoot(c.in, c.root, c.repl)
		if got != c.want {
			t.Errorf("rewriteRoot(%q, %q, %q) = %q, want %q", c.in, c.root, c.repl, got, c.want)
		}
	}
}

func TestSelect_RecognizesDangerous(t *testing.T) {
	rt, err := Select(HandlerReallyReallyDangerous, Options{})
	if err != nil {
		t.Fatalf("Select(dangerous): %v", err)
	}
	if _, ok := rt.(DangerouslyExecOnHost); !ok {
		t.Errorf("Select(dangerous) returned %T, want DangerouslyExecOnHost", rt)
	}
}

func TestSelect_RecognizesFirecracker(t *testing.T) {
	opts := Options{Firecracker: FirecrackerOptions{
		FirecrackerBin: "/usr/bin/firecracker",
		JailerBin:      "/usr/bin/jailer",
		KernelImage:    "/var/lib/hpcc/vmlinux",
		RootfsDir:      "/var/lib/hpcc/rootfs",
		RunDir:         "/srv/jailer",
		UID:            1000,
		GID:            1000,
	}}
	rt, err := Select(HandlerFirecracker, opts)
	if err != nil {
		t.Fatalf("Select(firecracker): %v", err)
	}
	if _, ok := rt.(*Firecracker); !ok {
		t.Errorf("Select(firecracker) returned %T, want *Firecracker", rt)
	}
}

func TestSelect_RejectsUnimplemented(t *testing.T) {
	for _, handler := range []string{"runhcs-wcow-hypervisor", "garbage"} {
		if _, err := Select(handler, Options{}); err == nil {
			t.Errorf("Select(%q) succeeded, want error", handler)
		}
	}
}
