package local

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDefaultsWhenAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, exists, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("exists should be false when no config file")
	}
	if cfg.HFRepo != "unsloth/gemma-4-12B-it-qat-GGUF:UD-Q4_K_XL" {
		t.Errorf("default HFRepo = %q", cfg.HFRepo)
	}
	if cfg.Port != 9280 || cfg.CtxSize != 262144 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestSaveLoadRoundTripAndReset(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	in := Defaults()
	in.Port = 9999
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	if !Exists() {
		t.Fatal("Exists should be true after Save")
	}
	out, exists, err := Load()
	if err != nil || !exists {
		t.Fatalf("Load err=%v exists=%v", err, exists)
	}
	if out.Port != 9999 {
		t.Errorf("Port = %d, want 9999", out.Port)
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if err := Reset(); err != nil {
		t.Fatal(err)
	}
	if Exists() {
		t.Fatal("Exists should be false after Reset")
	}
	// Reset must remove only local.json, not the entire state directory.
	if _, err := os.Stat(stateHome); err != nil {
		t.Fatalf("state dir removed by Reset: %v", err)
	}
}

func TestDropDeletesOnlyHFCachedModel(t *testing.T) {
	state := t.TempDir()
	cache := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLAMA_CACHE", cache)
	t.Setenv("HF_HUB_CACHE", t.TempDir())

	cfg := Defaults()
	cfg.HFRepo = "owner/repo:Q4_K_M"
	cfg.ModelName = "repo-Q4_K_M.gguf"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(cache, "repo-Q4_K_M.gguf")
	keepPath := filepath.Join(cache, "other.gguf")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Drop(); err != nil {
		t.Fatal(err)
	}
	if Exists() {
		t.Fatal("Drop should remove local config")
	}
	if _, err := os.Stat(modelPath); !os.IsNotExist(err) {
		t.Fatalf("downloaded model still exists or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("unrelated cache file removed: %v", err)
	}
}

func TestDropKeepsUserProvidedModelPath(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLAMA_CACHE", t.TempDir())
	t.Setenv("HF_HUB_CACHE", t.TempDir())

	modelPath := filepath.Join(t.TempDir(), "x.gguf")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.SourceKind = SourcePath
	cfg.ModelPath = modelPath
	cfg.ModelName = filepath.Base(modelPath)
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	if err := Drop(); err != nil {
		t.Fatal(err)
	}
	if Exists() {
		t.Fatal("Drop should remove local config")
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Fatalf("user-provided model path should be kept: %v", err)
	}
}

func TestBaseURL(t *testing.T) {
	c := Config{Host: "127.0.0.1", Port: 9280}
	if c.BaseURL() != "http://127.0.0.1:9280" {
		t.Errorf("BaseURL = %q", c.BaseURL())
	}
}

func TestBuildArgsHF(t *testing.T) {
	c := Defaults()
	args := c.BuildArgs()
	want := []string{
		"-hf", "unsloth/gemma-4-12B-it-qat-GGUF:UD-Q4_K_XL",
		"--host", "127.0.0.1", "--port", "9280", "-c", "262144",
		"-ngl", "999", "-fa", "on", "--jinja",
		"--spec-type", "draft-mtp", "--spec-draft-n-max", "4",
	}
	if !slices.Equal(args, want) {
		t.Errorf("BuildArgs =\n %v\nwant\n %v", args, want)
	}
}

func TestBuildArgsPathNoFlashAttn(t *testing.T) {
	c := Defaults()
	c.SourceKind = SourcePath
	c.ModelPath = "/models/x.gguf"
	c.FlashAttn = false
	c.ExtraArgs = nil
	args := c.BuildArgs()
	want := []string{
		"-m", "/models/x.gguf",
		"--host", "127.0.0.1", "--port", "9280", "-c", "262144",
		"-ngl", "999", "--jinja",
	}
	if !slices.Equal(args, want) {
		t.Errorf("BuildArgs =\n %v\nwant\n %v", args, want)
	}
}

func TestInstallHint(t *testing.T) {
	hint := InstallHint()
	if hint == "" {
		t.Fatal("InstallHint returned empty")
	}
	if !strings.Contains(hint, "llama.cpp") {
		t.Errorf("hint should mention llama.cpp: %q", hint)
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	c.BinaryPath = "/definitely/not/a/binary/llama-server"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "llama.cpp") {
		t.Errorf("missing-binary error should include install guidance: %v", err)
	}
	c2 := Defaults()
	c2.BinaryPath = "sh" // always present on the test host
	c2.SourceKind = SourceHF
	c2.HFRepo = ""
	if err := c2.Validate(); err == nil {
		t.Error("expected error for empty hf_repo")
	}
}
