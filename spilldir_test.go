//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// noCacheDir leaves the process without a user cache directory, as without
// HOME, and with a new temp directory, which it returns.
func noCacheDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("TMPDIR", tmp)
	if dir, err := os.UserCacheDir(); err == nil {
		t.Fatalf("os.UserCacheDir() = %s, want an error", dir)
	}
	return tmp
}

// fallbackDir is where spill files go without a user cache directory.
func fallbackDir(tmp string) string {
	return filepath.Join(tmp, "mcp-postgres-"+strconv.Itoa(os.Geteuid()))
}

func TestSpillDirIsUnderTheUserCacheDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := spillDir(); err != nil || got != filepath.Join(cache, "mcp-postgres") {
		t.Errorf("spillDir() = %q, %v; want mcp-postgres under %s", got, err, cache)
	}
}

// Without one, spill files go to a directory of this user's in the temp
// directory, which spillDir creates, then accepts.
func TestSpillDirFallsBackToAPrivateTempDirectory(t *testing.T) {
	want := fallbackDir(noCacheDir(t))
	for range 2 {
		if got, err := spillDir(); err != nil || got != want {
			t.Fatalf("spillDir() = %q, %v; want %s", got, err, want)
		}
	}
	info, err := os.Lstat(want)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("%s: %v, %v; want a directory with mode 0700", want, info, err)
	}
}

// Another user can create that path first in a shared temp directory, so a
// directory others may use, or a symlink, is refused.
func TestSpillDirRefusesATempDirectoryOthersCouldUse(t *testing.T) {
	dir := fallbackDir(noCacheDir(t))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the mode under test
		t.Fatal(err)
	}
	wantRefused(t)

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	wantRefused(t)
}

func wantRefused(t *testing.T) {
	t.Helper()
	if dir, err := spillDir(); err == nil || !strings.Contains(err.Error(), "must be a directory of this user's with mode 0700") {
		t.Errorf("spillDir() = %q, %v; want it refused", dir, err)
	}
}

func TestPrivateChecksTheOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !private(info, os.Geteuid()) {
		t.Error("private(own directory) = false")
	}
	if private(info, os.Geteuid()+1) {
		t.Error("private(another user's directory) = true")
	}
}

// Without a user cache directory, the server used to stop at startup.
func TestServerStartsWithoutAUserCacheDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the server")
	}
	bin := buildServer(t, map[string]any{"connections": map[string]any{}})
	tmp := noCacheDir(t) // after the build, which caches under HOME
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr.Close() }()
	cmd := exec.Command(bin) //nolint:gosec // the binary this test just built
	cmd.Stderr = stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "test"}, nil)
	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err == nil {
		_, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_list_connections"})
		err = errors.Join(err, session.Close())
	}
	if err != nil {
		logs, _ := os.ReadFile(stderr.Name()) //nolint:gosec // the file this test created
		t.Fatalf("server: %v\nstderr:\n%s", err, logs)
	}
	if info, err := os.Lstat(fallbackDir(tmp)); err != nil || !info.IsDir() {
		t.Errorf("spill directory %s: %v, %v", fallbackDir(tmp), info, err)
	}
}
