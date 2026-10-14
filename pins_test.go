package main

import (
	"regexp"
	"testing"
)

// Every action the workflows use is pinned to a commit, every tool they
// install to a version and the image's base to a digest, so a moved tag
// cannot change what CI or a release runs.
func TestWorkflowsAndImagePinWhatTheyRun(t *testing.T) {
	uses := regexp.MustCompile(`(?m)^\s+(?:- )?uses: (\S+)`)
	commit := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	versions := regexp.MustCompile(`(?m)^\s+version: (.+)$`)
	exact := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		text := readText(t, path)
		actions := uses.FindAllStringSubmatch(text, -1)
		if len(actions) == 0 {
			t.Errorf("%s uses no action", path)
		}
		for _, m := range actions {
			if !commit.MatchString(m[1]) {
				t.Errorf("%s uses %s, want an action pinned to a commit SHA", path, m[1])
			}
		}
		for _, m := range versions.FindAllStringSubmatch(text, -1) {
			if !exact.MatchString(m[1]) {
				t.Errorf("%s installs version %s, want an exact version such as v2.18.3", path, m[1])
			}
		}
	}
	if from := regexp.MustCompile(`(?m)^FROM (\S+)$`).FindStringSubmatch(readText(t, "Dockerfile")); from == nil ||
		!regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(from[1]) {
		t.Errorf("Dockerfile builds FROM %q, want an image pinned to a digest", from)
	}
}
