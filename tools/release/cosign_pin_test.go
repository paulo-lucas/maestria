package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// setup-cloud.sh installs cosign from a pinned release, and the release workflow pins a cosign
// too. They must be the same version, and the script's checksums must be real SHA-256 values.
func TestSetupCloudPinsTheSameCosignAsTheReleaseWorkflow(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "packaging", "linux", "setup-cloud.sh"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	scriptVersion := regexp.MustCompile(`(?m)^COSIGN_VERSION=(v\d+\.\d+\.\d+)$`).FindSubmatch(script)
	workflowVersion := regexp.MustCompile(`cosign-release:\s*(v\d+\.\d+\.\d+)`).FindSubmatch(workflow)
	if scriptVersion == nil || workflowVersion == nil {
		t.Fatalf("cosign version not found (script %v, workflow %v)", scriptVersion != nil, workflowVersion != nil)
	}
	if string(scriptVersion[1]) != string(workflowVersion[1]) {
		t.Errorf("setup-cloud.sh pins cosign %s, the release workflow %s", scriptVersion[1], workflowVersion[1])
	}
	for _, name := range []string{"COSIGN_SHA256_AMD64", "COSIGN_SHA256_ARM64"} {
		if !regexp.MustCompile(`(?m)^` + name + `=[0-9a-f]{64}$`).Match(script) {
			t.Errorf("%s is not a pinned SHA-256", name)
		}
	}
	if !regexp.MustCompile(`--no-cosign\)\s+INSTALL_COSIGN=0`).Match(script) {
		t.Error("the --no-cosign opt-out is missing")
	}
}
