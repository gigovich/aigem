package mcp

import (
	"context"
	"fmt"

	"github.com/gigovich/aigem/internal/config"
)

// Login runs the interactive OAuth flow for one configured http server by
// connecting to it (which 401s and triggers authorization), so the user can
// authenticate ahead of a session rather than mid-turn. The obtained token is
// persisted for reuse.
func Login(ctx context.Context, cwd, version, name string) error {
	cfgs, _ := loadConfigs(config.MCPFiles(cwd))
	var found *namedConfig
	for i := range cfgs {
		if cfgs[i].name == name {
			found = &cfgs[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no MCP server %q configured", name)
	}
	if found.cfg.URL == "" {
		return fmt.Errorf("server %q is not an http server; OAuth applies to url servers only", name)
	}
	cfg := found.cfg
	cfg.OAuth = true

	t, err := transportFor(name, cfg)
	if err != nil {
		return err
	}
	m := newManager(version)
	m.addServer(name, cfg, t)
	m.Connect(ctx)
	defer m.Close()
	if sc := m.byName[name]; sc.err != nil {
		return fmt.Errorf("login failed: %w", sc.err)
	}
	return nil
}

// Logout discards the persisted OAuth token (and registered client) for name.
func Logout(name string) error {
	return deleteOAuthState(name)
}
