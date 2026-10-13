package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	releaseTag     = "v1.2.3"
	releaseVersion = "1.2.3"
)

var releaseTargets = []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"}

type fakeRelease struct {
	files  map[string][]byte
	latest bool
}

func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()
	r := &fakeRelease{files: map[string][]byte{}, latest: true}
	for _, target := range releaseTargets {
		r.files[archiveName(target)] = tarball(t, fakeBinary(target))
	}
	r.writeChecksums(releaseTargets)
	return r
}

func archiveName(target string) string {
	return "pgglass_" + releaseVersion + "_" + target + ".tar.gz"
}

func fakeBinary(target string) string {
	return "#!/bin/sh\necho " + target + "\n"
}

func tarball(t *testing.T, binary string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{{"LICENSE", "MIT\n", 0o644}, {"pgglass", binary, 0o755}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (r *fakeRelease) writeChecksums(targets []string) {
	var sums strings.Builder
	for _, target := range targets {
		sum := sha256.Sum256(r.files[archiveName(target)])
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + archiveName(target) + "\n")
	}
	r.files["checksums.txt"] = []byte(sums.String())
}

func (r *fakeRelease) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	if r.latest {
		mux.Handle("/latest", http.RedirectHandler("/tag/"+releaseTag, http.StatusFound))
		mux.HandleFunc("/tag/"+releaseTag, func(http.ResponseWriter, *http.Request) {})
	}
	mux.HandleFunc("/download/"+releaseTag+"/{name}", func(w http.ResponseWriter, req *http.Request) {
		data, ok := r.files[req.PathValue("name")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runInstall(t *testing.T, releasesURL, system, machine string, env ...string) (string, error) {
	t.Helper()
	fakes := t.TempDir()
	uname := "#!/bin/sh\ncase $1 in -s) echo " + system + " ;; -m) echo " + machine + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(fakes, "uname"), []byte(uname), 0o755); err != nil { //nolint:gosec // a fake uname must be executable
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PGGLASS_RELEASES_URL="+releasesURL,
		"PGGLASS_VERSION=",
		"PGGLASS_INSTALL_DIR=",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func checkInstalled(t *testing.T, path, target string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pgglass not installed: %v", err)
	}
	if string(data) != fakeBinary(target) {
		t.Errorf("installed %q, want the %s binary", data, target)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("installed binary: %v, %v; want mode 0755", info, err)
	}
}

func checkNotInstalled(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "pgglass")); !os.IsNotExist(err) {
		t.Errorf("pgglass installed in %s anyway (%v)", dir, err)
	}
}

func TestInstallScriptInstallsTheLatestReleaseForThePlatform(t *testing.T) {
	releasesURL := newFakeRelease(t).serve(t)
	for _, tc := range []struct{ system, machine, target string }{
		{"Darwin", "arm64", "darwin_arm64"},
		{"Darwin", "x86_64", "darwin_amd64"},
		{"Linux", "aarch64", "linux_arm64"},
		{"Linux", "x86_64", "linux_amd64"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			home := t.TempDir()
			out, err := runInstall(t, releasesURL, tc.system, tc.machine, "HOME="+home)
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			checkInstalled(t, filepath.Join(home, ".local", "bin", "pgglass"), tc.target)
			if !strings.Contains(out, "Installed pgglass "+releaseTag) {
				t.Errorf("output does not name the release:\n%s", out)
			}
		})
	}
}

func TestInstallScriptInstallsTheReleaseAskedFor(t *testing.T) {
	release := newFakeRelease(t)
	release.latest = false
	dir := filepath.Join(t.TempDir(), "bin")
	out, err := runInstall(t, release.serve(t), "Linux", "amd64",
		"HOME="+t.TempDir(), "PGGLASS_VERSION="+releaseVersion, "PGGLASS_INSTALL_DIR="+dir)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	checkInstalled(t, filepath.Join(dir, "pgglass"), "linux_amd64")
	if !strings.Contains(out, "Add "+dir+" to your PATH") {
		t.Errorf("output does not say to add %s to PATH:\n%s", dir, out)
	}
}

func TestInstallScriptFailsWithoutALatestRelease(t *testing.T) {
	release := newFakeRelease(t)
	release.latest = false
	dir := t.TempDir()
	out, err := runInstall(t, release.serve(t), "Darwin", "arm64", "HOME="+t.TempDir(), "PGGLASS_INSTALL_DIR="+dir)
	if err == nil || !strings.Contains(out, "cannot find the latest release") {
		t.Errorf("install.sh: %v, want a failure saying it cannot find the latest release:\n%s", err, out)
	}
	checkNotInstalled(t, dir)
}

func TestInstallScriptRefusesAnArchiveItCannotVerify(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		tamper     func(*testing.T, *fakeRelease)
	}{
		{"checksum mismatch", "checksum mismatch for " + archiveName("linux_arm64"), func(t *testing.T, r *fakeRelease) {
			r.files[archiveName("linux_arm64")] = tarball(t, "#!/bin/sh\necho tampered\n")
		}},
		{"missing from checksums.txt", "lists no " + archiveName("linux_arm64"), func(_ *testing.T, r *fakeRelease) {
			r.writeChecksums(slices.DeleteFunc(slices.Clone(releaseTargets), func(s string) bool { return s == "linux_arm64" }))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := newFakeRelease(t)
			tc.tamper(t, release)
			dir := t.TempDir()
			out, err := runInstall(t, release.serve(t), "Linux", "arm64", "HOME="+t.TempDir(), "PGGLASS_INSTALL_DIR="+dir)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Errorf("install.sh: %v, want a failure saying %q:\n%s", err, tc.want, out)
			}
			checkNotInstalled(t, dir)
		})
	}
}

func TestInstallScriptRefusesUnsupportedPlatforms(t *testing.T) {
	releasesURL := newFakeRelease(t).serve(t)
	for _, tc := range []struct{ system, machine, want string }{
		{"MINGW64_NT-10.0-26100", "x86_64", "unsupported system MINGW64_NT-10.0-26100"},
		{"FreeBSD", "amd64", "unsupported system FreeBSD"},
		{"Linux", "i686", "unsupported architecture i686"},
	} {
		t.Run(tc.system+"/"+tc.machine, func(t *testing.T) {
			dir := t.TempDir()
			out, err := runInstall(t, releasesURL, tc.system, tc.machine, "HOME="+t.TempDir(), "PGGLASS_INSTALL_DIR="+dir)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Errorf("install.sh: %v, want a failure saying %q:\n%s", err, tc.want, out)
			}
			checkNotInstalled(t, dir)
		})
	}
}
