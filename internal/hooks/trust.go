package hooks

import (
	"fmt"

	projecttrust "github.com/gigovich/aigem/internal/trust"
)

const hookTrustTarget = "project-hooks"

func hookFingerprint(project map[string][]Matcher, disabled bool) (string, error) {
	return projecttrust.Fingerprint(struct {
		Hooks    map[string][]Matcher `json:"hooks"`
		Disabled bool                 `json:"disableAllHooks"`
	}{Hooks: project, Disabled: disabled})
}

func hookTrustStatus(dir, fingerprint string) (projecttrust.Status, error) {
	if err := projecttrust.MigrateLegacy(dir, projecttrust.CapabilityHooks, []projecttrust.CurrentTarget{{
		Target: hookTrustTarget, Fingerprint: fingerprint,
	}}); err != nil {
		return projecttrust.Status{}, fmt.Errorf("migrate legacy project hook trust: %w", err)
	}
	status, err := projecttrust.Evaluate(dir, projecttrust.CapabilityHooks, hookTrustTarget, fingerprint)
	if err != nil {
		return projecttrust.Status{}, fmt.Errorf("evaluate project hook trust: %w", err)
	}
	return status, nil
}

func approveHooks(dir, fingerprint string) error {
	if err := projecttrust.Approve(dir, projecttrust.CapabilityHooks, hookTrustTarget, fingerprint, "user"); err != nil {
		return fmt.Errorf("approve project hooks: %w", err)
	}
	return nil
}
