package compiler

import (
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		argv0      string
		wantName   string
		wantFamily enum.Family
		wantErr    bool
	}{
		{"clang", "clang", enum.GNUFamily, false},
		{"clang++", "clang++", enum.GNUFamily, false},
		{"/usr/bin/clang", "clang", enum.GNUFamily, false},
		{"./clang.exe", "clang", enum.GNUFamily, false},
		{"cl", "cl", enum.MSVCFamily, false},
		{"cl.exe", "cl", enum.MSVCFamily, false},
		{`C:\VC\bin\CL.EXE`, "cl", enum.MSVCFamily, false},
		{"gcc", "", 0, true}, // intentionally unsupported in v1
		{"nonsense", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.argv0, func(t *testing.T) {
			c, err := Detect(tc.argv0)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", tc.argv0)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Name() != tc.wantName {
				t.Errorf("Name = %q, want %q", c.Name(), tc.wantName)
			}
			if c.Family() != tc.wantFamily {
				t.Errorf("Family = %v, want %v", c.Family(), tc.wantFamily)
			}
		})
	}
}

func TestDetectDispatchesToCorrectParser(t *testing.T) {
	clang, _ := Detect("clang")
	inv, err := clang.Parse([]string{"-c", "foo.c", "-o", "foo.o"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Output != "foo.o" {
		t.Errorf("clang.Parse: Output = %q", inv.Output)
	}

	cl, _ := Detect("cl.exe")
	inv, err = cl.Parse([]string{"/c", "/Fo:foo.obj", "foo.cpp"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Output != "foo.obj" {
		t.Errorf("cl.Parse: Output = %q", inv.Output)
	}
}
