package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The release workflow's gate config is written by hand (setup reports NO_PROJECT on a Go
// repository), so these tests keep it valid as the config format evolves.
var releaseGate = filepath.Join("..", "..", ".github", "release-gate.yaml")

func TestReleaseGateConfigLoads(t *testing.T) {
	cfg, err := Load(releaseGate, env(map[string]string{"CANDIDATE_DIR": "/c"}), now)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the repository root %s has no go.mod: %v", root, err)
	}
	if cfg.Project != "shearbolt-release" {
		t.Errorf("project = %q, want shearbolt-release", cfg.Project)
	}
	binaries := []struct{ name, triple string }{
		{"linux-amd64", "x86_64-unknown-linux-gnu"},
		{"linux-arm64", "aarch64-unknown-linux-gnu"},
		{"darwin-amd64", "x86_64-apple-darwin"},
		{"darwin-arm64", "aarch64-apple-darwin"},
	}
	if len(cfg.Targets) != len(binaries)+1 {
		t.Fatalf("got %d targets, want %d: %+v", len(cfg.Targets), len(binaries)+1, cfg.Targets)
	}
	for i, b := range binaries {
		got := cfg.Targets[i]
		want := Target{Name: b.name, File: "/c/" + b.triple + "/shearbolt"}
		if got.Kind() != "file" || got.Name != want.Name || got.File != want.File || len(got.Exclude) != 0 || got.LockfileFallback != "" {
			t.Errorf("target %d = %+v, want %+v", i, got, want)
		}
	}
	repo := cfg.Targets[len(binaries)]
	if repo.Name != "repo" || repo.Kind() != "source" || repo.Source != root || !slices.Equal(repo.Exclude, []string{"testdata/"}) {
		t.Errorf("target %d = %+v, want source %s excluding testdata/", len(binaries), repo, root)
	}
	if cfg.Policy != DefaultPolicy() {
		t.Errorf("policy = %+v, want the default %+v", cfg.Policy, DefaultPolicy())
	}
	if len(cfg.Exceptions) != 0 {
		t.Errorf("exceptions = %+v, want none", cfg.Exceptions)
	}
}

// An unset CANDIDATE_DIR is a config error (exit 2), so the gate fails closed.
func TestReleaseGateConfigNeedsCandidateDir(t *testing.T) {
	_, err := Load(releaseGate, env(nil), now)
	if err == nil {
		t.Fatal("Load succeeded without CANDIDATE_DIR")
	}
	if !strings.Contains(err.Error(), "CANDIDATE_DIR") {
		t.Fatalf("error does not name CANDIDATE_DIR: %v", err)
	}
}
