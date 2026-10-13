//go:build unix

package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The built binary prints the version goreleaser links into it.
func TestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := filepath.Join(t.TempDir(), "pgglass")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=9.8.7", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").Output() //nolint:gosec
	if err != nil || string(out) != "pgglass 9.8.7\n" {
		t.Errorf("--version = %q, %v; want pgglass 9.8.7", out, err)
	}
}
