package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type registryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Packages    []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
	} `json:"packages"`
}

func readRegistryEntry(t *testing.T) registryEntry {
	t.Helper()
	data, err := os.ReadFile("server.json")
	if err != nil {
		t.Fatal(err)
	}
	var entry registryEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("server.json: %v", err)
	}
	return entry
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServerJSONNamesTheImageOfItsVersion(t *testing.T) {
	entry := readRegistryEntry(t)
	if n := len(entry.Description); n == 0 || n > 100 {
		t.Errorf("description is %d characters; the MCP Registry takes 1 to 100", n)
	}
	if entry.Version == "" || entry.Version == "latest" || strings.HasPrefix(entry.Version, "v") {
		t.Errorf("version %q; want a release version without the v, such as 1.0.0", entry.Version)
	}
	if len(entry.Packages) == 0 {
		t.Fatal("server.json lists no package")
	}
	goreleaser, release := readText(t, ".goreleaser.yml"), readText(t, ".github/workflows/release.yml")
	for _, pkg := range entry.Packages {
		image, tag, _ := strings.Cut(pkg.Identifier, ":")
		if pkg.RegistryType != "oci" || tag != entry.Version {
			t.Errorf("package %s %s; want an oci image tagged %s", pkg.RegistryType, pkg.Identifier, entry.Version)
		}
		if !strings.Contains(goreleaser, "\n      - "+image+"\n") || !strings.Contains(release, "IMAGE: "+image+"\n") {
			t.Errorf("image %s is not the one .goreleaser.yml publishes and release.yml attests", image)
		}
	}
}

func TestReleaseLabelsTheImageWithTheServerName(t *testing.T) {
	entry := readRegistryEntry(t)
	label := "io.modelcontextprotocol.server.name: " + entry.Name + "\n"
	if !strings.Contains(readText(t, ".goreleaser.yml"), label) {
		t.Errorf(".goreleaser.yml lacks the label %q, which the MCP Registry matches against server.json", strings.TrimSpace(label))
	}
}
