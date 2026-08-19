// Package bot runs the aigem harness as a long-lived, role-driven chat bot.
package bot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
)

// Config is one bot's on-disk definition (~/.config/aigem/bots/<name>/bot.yaml).
// The bot token is never stored here; it lives in the auth secret store.
type Config struct {
	Name              string         `yaml:"name"`
	Role              string         `yaml:"role"`
	Persona           string         `yaml:"persona,omitempty"` // e.g. "female; speaks Russian with feminine forms"
	Model             string         `yaml:"model,omitempty"`   // "provider/id"; empty inherits the role default
	Workdir           string         `yaml:"workdir"`
	CapabilityProfile string         `yaml:"capabilityProfile,omitempty"`
	TurnBudget        TurnBudgetConf `yaml:"turnBudget,omitempty"`
	LLMPaceFactor     *float64       `yaml:"llmPaceFactor,omitempty"`
	Cron              []CronJob      `yaml:"cron,omitempty"`
}

const (
	DefaultArchitectModel = "openai/gpt-5.6-sol"
	DefaultBotModel       = "openai/gpt-5.6-luna"

	ModelSourceConfigured  = "configured"
	ModelSourceRoleDefault = "role-default"
)

// ModelSelection describes both the persisted override and the model a bot must
// open. Configured stays empty for inherited defaults so reading a config never
// turns inheritance into a pin.
type ModelSelection struct {
	Configured string
	Effective  string
	Source     string
}

// DefaultModelForRole returns the binding default for a built-in role. Unknown
// roles fail instead of silently inheriting a model intended for valid bots.
func DefaultModelForRole(role string) (string, error) {
	role = strings.TrimSpace(role)
	if _, ok := RoleByName(role); !ok {
		return "", fmt.Errorf("unknown role %q", role)
	}
	if role == "architect" {
		return DefaultArchitectModel, nil
	}
	return DefaultBotModel, nil
}

// ModelSelection resolves an explicit override or the built-in role default.
func (c Config) ModelSelection() (ModelSelection, error) {
	configured := strings.TrimSpace(c.Model)
	if configured != "" {
		return ModelSelection{Configured: configured, Effective: configured, Source: ModelSourceConfigured}, nil
	}
	effective, err := DefaultModelForRole(c.Role)
	if err != nil {
		return ModelSelection{}, err
	}
	return ModelSelection{Effective: effective, Source: ModelSourceRoleDefault}, nil
}

// DefaultLLMPaceFactor throttles an unattended bot's LLM request rate: after each
// call the bot waits this multiple of the call's wall-clock duration before the
// next request. 1.0 roughly halves throughput (each call is followed by an equal
// pause), keeping bots well under provider rate limits.
const DefaultLLMPaceFactor = 1.0

// ResolveLLMPaceFactor returns the configured pace factor, or the default when
// unset. A zero (or negative) value disables pacing.
func (c Config) ResolveLLMPaceFactor() float64 {
	if c.LLMPaceFactor == nil {
		return DefaultLLMPaceFactor
	}
	return *c.LLMPaceFactor
}

// TurnBudgetConf customizes per-turn runaway protection for unattended bot runs.
// Nil fields inherit the default unattended budget. Explicit integer zero values
// disable that budget. MaxDuration is a Go duration string such as "20m" or
// "1h"; use "0" to disable the wall-clock budget.
type TurnBudgetConf struct {
	MaxModelRounds       *int   `yaml:"maxModelRounds,omitempty"`
	MaxToolCalls         *int   `yaml:"maxToolCalls,omitempty"`
	MaxRepeatedToolCalls *int   `yaml:"maxRepeatedToolCalls,omitempty"`
	MaxDuration          string `yaml:"maxDuration,omitempty"`
}

// TurnBudgetForRole returns the base per-turn budget for a role, before any bot overrides. The
// developer role implements whole tickets in a turn (read many files, edit several, build, test,
// iterate), which easily exceeds the default round cap, so it gets a larger allowance; other roles
// use the default. A genuine runaway is still caught by the repeated-tool-call guard independently.
func TurnBudgetForRole(role string) agent.TurnBudget {
	b := agent.DefaultTurnBudget()
	if role == "developer" {
		b.MaxModelRounds = 120
		b.MaxToolCalls = 300
		b.MaxDuration = 45 * time.Minute
	}
	return b
}

// ResolveTurnBudget returns the default unattended budget with any configured bot overrides applied.
func (c TurnBudgetConf) ResolveTurnBudget() (agent.TurnBudget, error) {
	return c.resolve(agent.DefaultTurnBudget())
}

// ResolveTurnBudgetFor applies the configured bot overrides on top of a role-specific base budget.
func (c TurnBudgetConf) ResolveTurnBudgetFor(base agent.TurnBudget) (agent.TurnBudget, error) {
	return c.resolve(base)
}

func (c TurnBudgetConf) resolve(b agent.TurnBudget) (agent.TurnBudget, error) {
	if c.MaxModelRounds != nil {
		b.MaxModelRounds = *c.MaxModelRounds
	}
	if c.MaxToolCalls != nil {
		b.MaxToolCalls = *c.MaxToolCalls
	}
	if c.MaxRepeatedToolCalls != nil {
		b.MaxRepeatedToolCalls = *c.MaxRepeatedToolCalls
	}
	if c.MaxDuration != "" {
		if c.MaxDuration == "0" {
			b.MaxDuration = 0
		} else {
			d, err := time.ParseDuration(c.MaxDuration)
			if err != nil {
				return agent.TurnBudget{}, fmt.Errorf("parse turnBudget.maxDuration: %w", err)
			}
			b.MaxDuration = d
		}
	}
	return b, nil
}

// CronJob is one scheduled task. A recurring job carries a 5-field cron Expr; a one-shot job
// carries At (an RFC3339 instant) instead and self-deletes after it fires. Exactly one of Expr/At
// is set.
type CronJob struct {
	ID     string `yaml:"id"`
	Expr   string `yaml:"expr,omitempty"`
	At     string `yaml:"at,omitempty"`
	Prompt string `yaml:"prompt"`
}

// Dir returns ~/.config/aigem/bots/<name>.
func Dir(name string) (string, error) {
	base, err := config.BotsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name), nil
}

// MemoryDir and SkillsDir are the per-bot memory and self-skills roots (used by later slices).
func MemoryDir(name string) (string, error) { return sub(name, "memory") }
func SkillsDir(name string) (string, error) { return sub(name, "skills") }

func sub(name, child string) (string, error) {
	d, err := Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, child), nil
}

// Save writes bot.yaml, creating the bot directory if needed.
func Save(c Config) error {
	if c.Name == "" {
		return fmt.Errorf("bot name is required")
	}
	dir, err := Dir(c.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// Write through a uniquely named temporary file: bot.yaml is edited from the
	// CLI (`aigem bot model`) while the bot itself rewrites it to persist cron
	// jobs, so a crash - or two writers sharing one fixed temp path - would
	// otherwise leave a truncated config that no longer starts the bot.
	path := filepath.Join(dir, "bot.yaml")
	tmp, err := os.CreateTemp(dir, ".bot.yaml.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Carry over the existing file's permissions; a config an operator hardened
	// must not be widened by a routine model switch. A new file keeps
	// CreateTemp's owner-only mode.
	if info, serr := os.Stat(path); serr == nil {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads bot.yaml for the named bot.
func Load(name string) (Config, error) {
	dir, err := Dir(name)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "bot.yaml"))
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse bot.yaml: %w", err)
	}
	if c.Name == "" {
		c.Name = name
	}
	return c, nil
}

// Update serializes a bot.yaml read-modify-write across processes. The lock file
// is intentionally persistent: the operating system releases its advisory lock
// when a process exits, so a crash cannot leave stale ownership behind.
func Update(name string, mutate func(*Config) error) (saved Config, err error) {
	if name == "" {
		return Config{}, fmt.Errorf("bot name is required")
	}
	if mutate == nil {
		return Config{}, fmt.Errorf("bot config mutation is required")
	}
	dir, err := Dir(name)
	if err != nil {
		return Config{}, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".bot.yaml.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Config{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := lockBotConfig(lock); err != nil {
		return Config{}, fmt.Errorf("lock bot %q config: %w", name, err)
	}
	defer func() { err = errors.Join(err, unlockBotConfig(lock)) }()

	current, err := Load(name)
	if err != nil {
		return Config{}, err
	}
	if err := mutate(&current); err != nil {
		return Config{}, err
	}
	current.Name = name
	if err := Save(current); err != nil {
		return Config{}, err
	}
	return current, nil
}

// List returns the names of all configured bots, sorted.
func List() ([]string, error) {
	base, err := config.BotsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Remove deletes a bot's directory and its stored token.
func Remove(name string) error {
	dir, err := Dir(name)
	if err != nil {
		return err
	}
	_ = DeleteToken(name)
	return os.RemoveAll(dir)
}

func tokenKey(name string) string { return "bot:" + name }

// DeleteToken removes any secret an older install stored for this bot.
//
// Nothing writes one any more: a bot reaches its conversations through the
// store the fleet process owns, so there is no chat credential to keep. `aigem
// bot rm` still calls this, because a Mattermost token left in the auth store
// after the transport that used it was deleted is a live credential nobody is
// watching.
func DeleteToken(name string) error { return auth.Delete(tokenKey(name)) }
