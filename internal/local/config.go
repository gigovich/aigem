// Package local manages the on-disk setup and llama-server daemon backing the
// built-in local model.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/gigovich/aigem/internal/config"
)

// Source kinds for SourceKind: download from Hugging Face or use a local path.
const (
	SourceHF   = "hf"
	SourcePath = "path"
)

// Config holds the persisted settings for the local llama-server daemon.
type Config struct {
	BinaryPath string   `json:"binary_path"`
	SourceKind string   `json:"source_kind"`
	HFRepo     string   `json:"hf_repo,omitempty"`
	ModelPath  string   `json:"model_path,omitempty"`
	ModelName  string   `json:"model_name"`
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	CtxSize    int      `json:"ctx_size"`
	NGL        int      `json:"ngl"`
	FlashAttn  bool     `json:"flash_attn"`
	ExtraArgs  []string `json:"extra_args,omitempty"`
}

// Defaults returns the built-in configuration targeting the Unsloth Gemma 4 12B
// QAT GGUF. The launch flags suit both Apple Silicon (Metal) and NVIDIA (CUDA);
// only the llama.cpp build differs.
func Defaults() Config {
	return Config{
		BinaryPath: "llama-server",
		SourceKind: SourceHF,
		HFRepo:     "unsloth/gemma-4-12B-it-qat-GGUF:UD-Q4_K_XL",
		ModelName:  "gemma-4-12B-it-qat-UD-Q4_K_XL.gguf",
		Host:       "127.0.0.1",
		Port:       9280,
		CtxSize:    262144,
		NGL:        999,
		FlashAttn:  true,
		ExtraArgs:  []string{"--spec-type", "draft-mtp", "--spec-draft-n-max", "4"},
	}
}

// fileMu guards concurrent CLI invocations reading and writing local.json.
var fileMu sync.Mutex

func configPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local.json"), nil
}

// Load reads the saved config; the bool is true when the file exists (setup has run).
func Load() (Config, bool, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	path, err := configPath()
	if err != nil {
		return Config{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Defaults(), false, nil
		}
		return Config{}, false, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, true, nil
}

// Save writes the config with 0600 perms.
func Save(c Config) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	path, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// WriteFile does not tighten perms on a pre-existing looser file; enforce 0600.
	return os.Chmod(path, 0o600)
}

// Exists reports whether a local config file is present, i.e. setup has been run.
func Exists() bool {
	path, err := configPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Reset stops the running server (if any) and removes the config file.
func Reset() error {
	if err := Stop(); err != nil {
		return err
	}
	return removeConfig()
}

// Drop stops the running server, removes the config file, and deletes the cached
// model files for HF-sourced models. User-provided local paths are never deleted.
func Drop() error {
	cfg, exists, err := Load()
	if err != nil {
		return err
	}
	if err := Stop(); err != nil {
		return err
	}
	if exists && cfg.SourceKind == SourceHF {
		if err := removeDownloadedModel(cfg); err != nil {
			return err
		}
	}
	return removeConfig()
}

func removeConfig() error {
	fileMu.Lock()
	defer fileMu.Unlock()
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeDownloadedModel(c Config) error {
	var errs []error
	for _, dir := range cacheDirs() {
		if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isDownloadedModelFile(c, p) {
				return nil
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			return nil
		}); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isDownloadedModelFile(c Config, path string) bool {
	if !strings.HasSuffix(strings.ToLower(path), ".gguf") {
		return false
	}
	base := strings.ToLower(filepath.Base(path))
	if c.ModelName != "" && base == strings.ToLower(c.ModelName) {
		return true
	}
	repo, quant, _ := strings.Cut(c.HFRepo, ":")
	if quant != "" && !strings.Contains(strings.ToLower(path), strings.ToLower(quant)) {
		return false
	}
	repoKey := "models--" + strings.ReplaceAll(strings.ToLower(repo), "/", "--")
	return repo != "" && strings.Contains(strings.ToLower(path), repoKey)
}

// BaseURL returns the http://host:port base URL for the llama-server HTTP API.
func (c Config) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

// Validate checks that the config is self-consistent and the binary is on PATH.
func (c Config) Validate() error {
	switch c.SourceKind {
	case SourceHF:
		if c.HFRepo == "" {
			return errors.New("hf_repo is required for source_kind=hf")
		}
	case SourcePath:
		if c.ModelPath == "" {
			return errors.New("model_path is required for source_kind=path")
		}
		if _, err := os.Stat(c.ModelPath); err != nil {
			return fmt.Errorf("model file %q: %w", c.ModelPath, err)
		}
	default:
		return fmt.Errorf("unknown source_kind %q", c.SourceKind)
	}
	if _, err := exec.LookPath(c.BinaryPath); err != nil {
		return fmt.Errorf("llama-server (%q) not found on PATH.\n\n%s", c.BinaryPath, InstallHint())
	}
	if c.Port <= 0 {
		return fmt.Errorf("port must be positive, got %d", c.Port)
	}
	return nil
}

// InstallHint returns OS-specific instructions for installing llama.cpp (which
// provides the llama-server binary), so a missing binary tells the user how to
// fix it rather than just that it is absent.
func InstallHint() string {
	const docs = "Then try again. Docs: https://github.com/ggml-org/llama.cpp/blob/master/docs/install.md"
	switch runtime.GOOS {
	case "darwin":
		return "Install llama.cpp, for example:\n" +
			"  brew install llama.cpp\n" +
			"  # or: sudo port install llama.cpp   (MacPorts)\n" +
			"  # or: nix profile install nixpkgs#llama-cpp\n" + docs
	case "linux":
		return "Install llama.cpp, for example:\n" +
			"  brew install llama.cpp                (Homebrew on Linux)\n" +
			"  # or: nix profile install nixpkgs#llama-cpp\n" +
			"  # or build from source: https://github.com/ggml-org/llama.cpp\n" + docs
	case "windows":
		return "Install llama.cpp, for example:\n" +
			"  winget install llama.cpp\n" +
			"  # or download a release: https://github.com/ggml-org/llama.cpp/releases\n" + docs
	default:
		return "Install llama.cpp so that 'llama-server' is on your PATH.\n" + docs
	}
}

// BuildArgs returns the CLI argument slice for llama-server; --jinja is always
// appended because Gemma native tool calling requires it.
func (c Config) BuildArgs() []string {
	var a []string
	if c.SourceKind == SourcePath {
		a = append(a, "-m", c.ModelPath)
	} else {
		a = append(a, "-hf", c.HFRepo)
	}
	a = append(a, "--host", c.Host, "--port", strconv.Itoa(c.Port), "-c", strconv.Itoa(c.CtxSize))
	if c.NGL > 0 {
		a = append(a, "-ngl", strconv.Itoa(c.NGL))
	}
	if c.FlashAttn {
		a = append(a, "-fa", "on")
	}
	a = append(a, "--jinja")
	a = append(a, c.ExtraArgs...)
	return a
}

// CommandString returns the full launch command as a human-readable string.
func (c Config) CommandString() string {
	return c.BinaryPath + " " + strings.Join(c.BuildArgs(), " ")
}
