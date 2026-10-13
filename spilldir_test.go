//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

func fallbackDir(tmp string) string {
	return filepath.Join(tmp, "pgglass-"+strconv.Itoa(os.Geteuid()))
}

func TestSpillDirIsUnderTheUserCacheDirectory(t *testing.T) {
	isolateHome(t)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := spillDir(); err != nil || got != filepath.Join(cache, "pgglass") {
		t.Errorf("spillDir() = %q, %v; want pgglass under %s", got, err, cache)
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
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec
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
	path := writeConfig(t, connections(map[string]any{}))
	tmp := noCacheDir(t)
	session, logs, done := connectStdio(t, "--config", path)
	if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_list_connections"}); err != nil {
		t.Errorf("pg_list_connections: %v\nstderr:\n%s", err, logs)
	}
	if err := session.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if code := exitCode(t, done, logs); code != 0 {
		t.Errorf("run = %d, want 0\nstderr:\n%s", code, logs)
	}
	if info, err := os.Lstat(fallbackDir(tmp)); err != nil || !info.IsDir() {
		t.Errorf("spill directory %s: %v, %v", fallbackDir(tmp), info, err)
	}
}

func TestSpillDirFailureStopsTheServer(t *testing.T) {
	path := writeConfig(t, connections(map[string]any{}))
	dir := fallbackDir(noCacheDir(t))
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCommand(t, "--config", path)
	if code != 1 || !strings.Contains(errOut, "spill directory: "+dir+" must be a directory of this user's") {
		t.Errorf("run = %d, %q; want 1 and the spill directory refused", code, errOut)
	}
}
