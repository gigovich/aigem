// Package trust persists capability-scoped approvals for project-origin artifacts.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/config"
)

// Capability identifies an independently approved project-origin risk surface.
type Capability string

const (
	CapabilityHooks     Capability = "hooks"
	CapabilitySkills    Capability = "skills"
	CapabilityMCPStdio  Capability = "mcp-stdio"
	CapabilityMCPHTTP   Capability = "mcp-http"
	CapabilityMCPPolicy Capability = "mcp-policy"
)

// State is the effective authorization state for one capability and target.
type State string

const (
	StateAllowed     State = "allowed"
	StateDenied      State = "denied"
	StatePending     State = "pending"
	StateInvalidated State = "invalidated"
)

// Status describes the effective authorization and the record that caused it.
type Status struct {
	State       State
	Project     string
	Capability  Capability
	Target      string
	Fingerprint string
	TrustedAt   time.Time
	TrustedBy   string
	Legacy      bool
}

type decision string

const (
	decisionAllow decision = "allow"
	decisionDeny  decision = "deny"
)

type record struct {
	Project     string     `json:"project"`
	Capability  Capability `json:"capability"`
	Target      string     `json:"target"`
	Fingerprint string     `json:"fingerprint"`
	Decision    decision   `json:"decision"`
	DecidedAt   time.Time  `json:"decidedAt"`
	DecidedBy   string     `json:"decidedBy"`
	Legacy      bool       `json:"legacy,omitempty"`
}

type migration struct {
	Project    string     `json:"project"`
	Capability Capability `json:"capability"`
}

type file struct {
	Version    int         `json:"version"`
	Records    []record    `json:"records"`
	Migrations []migration `json:"legacyMigrations,omitempty"`
}

// CurrentTarget is one currently declared target used during legacy migration.
type CurrentTarget struct {
	Target      string
	Fingerprint string
}

var mu sync.Mutex

// Fingerprint returns a stable SHA-256 fingerprint of a JSON-serializable value.
func Fingerprint(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Evaluate returns the authorization state for the current fingerprint.
func Evaluate(project string, capability Capability, target, fingerprint string) (Status, error) {
	if err := validate(capability, target, fingerprint); err != nil {
		return Status{}, err
	}
	project, err := canonicalProject(project)
	if err != nil {
		return Status{}, err
	}

	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return Status{}, err
	}
	if status, found := evaluateRecords(f.Records, project, capability, target, fingerprint); found {
		return status, nil
	}
	return newStatus(StatePending, project, capability, target, fingerprint, record{}), nil
}

// MigrateLegacy grants a legacy-approved project's targets that are declared
// now, then permanently marks the capability migrated. Targets introduced later
// stay pending instead of inheriting the old whole-project bit.
func MigrateLegacy(project string, capability Capability, targets []CurrentTarget) error {
	canonical, err := canonicalProject(project)
	if err != nil {
		return err
	}
	project = canonical
	switch capability {
	case CapabilityHooks, CapabilitySkills, CapabilityMCPStdio, CapabilityMCPHTTP, CapabilityMCPPolicy:
	default:
		return fmt.Errorf("unknown project trust capability %q", capability)
	}
	for _, target := range targets {
		if err := validate(capability, target.Target, target.Fingerprint); err != nil {
			return err
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Migration is a no-op on almost every startup, and every capability calls
	// this. Decide that from a plain read, so the common path never creates a
	// lock file - which would also turn an unwritable state dir into an error on
	// a path that used to be read-only.
	f, err := load()
	if err != nil {
		return err
	}
	for _, migrated := range f.Migrations {
		if migrated.Project == project && migrated.Capability == capability {
			return nil
		}
	}
	legacy, err := legacyApproved(project)
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	return withFileLock(func() error {
		f, err := load()
		if err != nil {
			return err
		}
		for _, migrated := range f.Migrations {
			if migrated.Project == project && migrated.Capability == capability {
				return nil
			}
		}
		now := time.Now().UTC()
		for _, target := range targets {
			if _, found := evaluateRecords(f.Records, project, capability, target.Target, target.Fingerprint); found {
				continue
			}
			f.Records = append(f.Records, record{
				Project: project, Capability: capability, Target: target.Target, Fingerprint: target.Fingerprint,
				Decision: decisionAllow, DecidedAt: now, DecidedBy: "legacy-project-trust", Legacy: true,
			})
		}
		f.Migrations = append(f.Migrations, migration{Project: project, Capability: capability})
		if err := save(f); err != nil {
			return fmt.Errorf("migrate legacy project trust: %w", err)
		}
		return nil
	})
}

// Approve allows the exact capability target at its current fingerprint.
func Approve(project string, capability Capability, target, fingerprint, by string) error {
	return decide(project, capability, target, fingerprint, by, decisionAllow)
}

// Revoke denies the exact capability target at its current fingerprint. A later
// approval is required to enable it again.
func Revoke(project string, capability Capability, target, fingerprint, by string) error {
	return decide(project, capability, target, fingerprint, by, decisionDeny)
}

func decide(project string, capability Capability, target, fingerprint, by string, d decision) error {
	if err := validate(capability, target, fingerprint); err != nil {
		return err
	}
	project, err := canonicalProject(project)
	if err != nil {
		return err
	}
	if by == "" {
		by = "user"
	}

	mu.Lock()
	defer mu.Unlock()
	return withFileLock(func() error {
		f, err := load()
		if err != nil {
			return err
		}
		kept := f.Records[:0]
		for _, r := range f.Records {
			if r.Project == project && r.Capability == capability && r.Target == target {
				continue
			}
			kept = append(kept, r)
		}
		f.Records = append(kept, record{
			Project: project, Capability: capability, Target: target, Fingerprint: fingerprint,
			Decision: d, DecidedAt: time.Now().UTC(), DecidedBy: by,
		})
		return save(f)
	})
}

func validate(capability Capability, target, fingerprint string) error {
	switch capability {
	case CapabilityHooks, CapabilitySkills, CapabilityMCPStdio, CapabilityMCPHTTP, CapabilityMCPPolicy:
	default:
		return fmt.Errorf("unknown project trust capability %q", capability)
	}
	if target == "" {
		return errors.New("project trust target is required")
	}
	if fingerprint == "" {
		return errors.New("project trust fingerprint is required")
	}
	return nil
}

func canonicalProject(project string) (string, error) {
	if project == "" {
		return "", errors.New("project path is required")
	}
	abs, err := filepath.Abs(project)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs), nil
}

func evaluateRecords(records []record, project string, capability Capability, target, fingerprint string) (Status, bool) {
	var latest *record
	for i := range records {
		r := &records[i]
		if r.Project != project || r.Capability != capability || r.Target != target {
			continue
		}
		if latest == nil || latest.DecidedAt.Before(r.DecidedAt) {
			latest = r
		}
	}
	if latest == nil {
		return Status{}, false
	}
	state := StateInvalidated
	if latest.Fingerprint == fingerprint {
		if latest.Decision == decisionAllow {
			state = StateAllowed
		} else {
			state = StateDenied
		}
	}
	return newStatus(state, project, capability, target, fingerprint, *latest), true
}

func newStatus(state State, project string, capability Capability, target, fingerprint string, r record) Status {
	return Status{
		State: state, Project: project, Capability: capability, Target: target, Fingerprint: fingerprint,
		TrustedAt: r.DecidedAt, TrustedBy: r.DecidedBy, Legacy: r.Legacy,
	}
}

func trustFile() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "project-trust.json"), nil
}

func load() (file, error) {
	path, err := trustFile()
	if err != nil {
		return file{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file{Version: 1}, nil
	}
	if err != nil {
		return file{}, fmt.Errorf("read project trust: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return file{}, fmt.Errorf("parse project trust: %w", err)
	}
	if f.Version != 1 {
		return file{}, fmt.Errorf("unsupported project trust version %d", f.Version)
	}
	return f, nil
}

func save(f file) error {
	path, err := trustFile()
	if err != nil {
		return err
	}
	f.Version = 1
	sort.Slice(f.Records, func(i, j int) bool {
		a, b := f.Records[i], f.Records[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Capability != b.Capability {
			return a.Capability < b.Capability
		}
		return a.Target < b.Target
	})
	sort.Slice(f.Migrations, func(i, j int) bool {
		if f.Migrations[i].Project != f.Migrations[j].Project {
			return f.Migrations[i].Project < f.Migrations[j].Project
		}
		return f.Migrations[i].Capability < f.Migrations[j].Capability
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// A unique temp name, not path+".tmp": two aigem processes approving at once
	// would otherwise write the same file and rename a half-written one into
	// place, which corrupts the store permanently since load refuses to parse it.
	dir := filepath.Dir(path)
	sweepOrphanTemps(dir)
	tmp, err := os.CreateTemp(dir, "project-trust-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// sweepOrphanTemps removes temp files left by a process killed between
// CreateTemp and Rename. Unlike the old fixed temp name these are never reused,
// so nothing else would ever clean them up.
func sweepOrphanTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "project-trust-") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < tempOrphan {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

// withFileLock runs fn while holding an exclusive lock on the trust file, so a
// read-modify-write from another aigem process cannot interleave and drop an
// approval. A lock left behind by a killed process is broken after lockStale.
func withFileLock(fn func() error) error {
	path, err := trustFile()
	if err != nil {
		return err
	}
	lock := path + ".lock"
	for waited := time.Duration(0); ; waited += lockPoll {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			defer os.Remove(lock)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock project trust: %w", err)
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > lockStale {
			os.Remove(lock)
			continue
		}
		if waited >= lockWait {
			// Proceeding unlocked risks a lost approval; corrupting the store does
			// not, since save renames a fully written file into place.
			return fn()
		}
		time.Sleep(lockPoll)
	}
}

const (
	lockPoll   = 20 * time.Millisecond
	lockWait   = 2 * time.Second
	lockStale  = 30 * time.Second
	tempOrphan = time.Hour
)

func legacyApproved(project string) (bool, error) {
	dir, err := config.StateDir()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "trusted-hooks.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read legacy project trust: %w", err)
	}
	var projects []string
	if err := json.Unmarshal(data, &projects); err != nil {
		return false, fmt.Errorf("parse legacy project trust: %w", err)
	}
	for _, candidate := range projects {
		canonical, err := canonicalProject(candidate)
		if err == nil && canonical == project {
			return true, nil
		}
	}
	return false, nil
}
