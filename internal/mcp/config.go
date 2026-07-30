// Package mcp connects aigem to Model Context Protocol servers and exposes
// their tools, prompts, and resources through the existing tool/command surface.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/gigovich/aigem/internal/config"
	projecttrust "github.com/gigovich/aigem/internal/trust"
)

// ServerConfig is one entry of the de-facto `mcpServers` map (Claude Code /
// Cursor compatible). The transport is inferred: URL means Streamable HTTP, a
// Command means stdio.
type ServerConfig struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	OAuth       bool              `json:"oauth,omitempty"` // enable interactive OAuth 2.0 (http only)
	Disabled    bool              `json:"disabled,omitempty"`
	AutoApprove []string          `json:"autoApprove,omitempty"`
}

// Transport returns a short label ("stdio" or "http") plus a one-line detail,
// for the `mcp list` command.
func (c ServerConfig) Transport() (string, string) {
	if c.URL != "" {
		return "http", c.URL
	}
	detail := c.Command
	if len(c.Args) > 0 {
		detail += " " + strings.Join(c.Args, " ")
	}
	return "stdio", detail
}

// ConfiguredServer is one merged server entry, for listing.
type ConfiguredServer struct {
	Name string
	Cfg  ServerConfig
}

// ListConfigured returns the effective (merged) MCP servers under cwd.
func ListConfigured(cwd string) ([]ConfiguredServer, []error) {
	cfgs, warns := Configs(cwd)
	out := make([]ConfiguredServer, 0, len(cfgs))
	for _, nc := range cfgs {
		out = append(out, ConfiguredServer{Name: nc.name, Cfg: nc.cfg})
	}
	return out, warns
}

// namedConfig pairs a server name with its config, preserving discovery order.
type namedConfig struct {
	name         string
	cfg          ServerConfig
	projectLocal bool
}

// loadConfigs reads the `mcpServers` block from each file (highest precedence
// first) and returns the merged servers in stable order. The first file to
// define a name wins, so project sources beat global. Malformed files yield a
// warning and are skipped rather than failing startup.
func loadConfigs(files []string) ([]namedConfig, []error) {
	return loadConfigsWithProject(files, "")
}

func loadConfigsWithProject(files []string, projectDir string) ([]namedConfig, []error) {
	var out []namedConfig
	var warns []error
	seen := map[string]bool{}
	prefix := ""
	if projectDir != "" {
		prefix = projectDir + string(os.PathSeparator)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			MCPServers map[string]ServerConfig `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			warns = append(warns, fmt.Errorf("%s: %w", f, err))
			continue
		}
		names := make([]string, 0, len(doc.MCPServers))
		for name := range doc.MCPServers {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic order within a file
		for _, name := range names {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, namedConfig{
				name:         name,
				cfg:          doc.MCPServers[name],
				projectLocal: prefix != "" && strings.HasPrefix(f, prefix),
			})
		}
	}
	return out, warns
}

// Configs returns the configured MCP servers discovered under cwd, in
// precedence order, plus warnings for any malformed config files.
func Configs(cwd string) ([]namedConfig, []error) {
	return loadConfigs(config.MCPFiles(cwd))
}

// RuntimeConfigs returns the MCP servers safe to start now. Project-local MCP
// servers are withheld until the project is trusted; global servers keep their
// existing behavior.
func RuntimeConfigs(cwd string, trustProject bool) ([]namedConfig, []error) {
	projectDir := config.ProjectDir(cwd)
	cfgs, warns := loadConfigsWithProject(config.MCPFiles(cwd), projectDir)
	legacyTargets := map[projecttrust.Capability][]projecttrust.CurrentTarget{
		projecttrust.CapabilityMCPStdio:  nil,
		projecttrust.CapabilityMCPHTTP:   nil,
		projecttrust.CapabilityMCPPolicy: nil,
	}
	for _, nc := range cfgs {
		if !nc.projectLocal {
			continue
		}
		transport, _ := nc.cfg.Transport()
		capability := projecttrust.CapabilityMCPStdio
		if transport == "http" {
			capability = projecttrust.CapabilityMCPHTTP
		}
		if fingerprint, err := mcpTransportFingerprint(nc.cfg); err == nil {
			legacyTargets[capability] = append(legacyTargets[capability], projecttrust.CurrentTarget{Target: nc.name, Fingerprint: fingerprint})
		}
		if len(nc.cfg.AutoApprove) > 0 {
			if fingerprint, err := mcpPolicyFingerprint(nc.name, nc.cfg.AutoApprove); err == nil {
				legacyTargets[projecttrust.CapabilityMCPPolicy] = append(legacyTargets[projecttrust.CapabilityMCPPolicy], projecttrust.CurrentTarget{Target: nc.name, Fingerprint: fingerprint})
			}
		}
	}
	for _, capability := range []projecttrust.Capability{projecttrust.CapabilityMCPStdio, projecttrust.CapabilityMCPHTTP, projecttrust.CapabilityMCPPolicy} {
		if err := projecttrust.MigrateLegacy(projectDir, capability, legacyTargets[capability]); err != nil {
			warns = append(warns, fmt.Errorf("migrate legacy %s trust: %w", capability, err))
		}
	}
	out := make([]namedConfig, 0, len(cfgs))
	for _, nc := range cfgs {
		if !nc.projectLocal {
			out = append(out, nc)
			continue
		}
		transport, _ := nc.cfg.Transport()
		capability := projecttrust.CapabilityMCPStdio
		if transport == "http" {
			capability = projecttrust.CapabilityMCPHTTP
		}
		fingerprint, err := mcpTransportFingerprint(nc.cfg)
		if err != nil {
			warns = append(warns, fmt.Errorf("fingerprint project-local %s MCP server %q: %w", transport, nc.name, err))
			continue
		}
		if trustProject {
			if err := projecttrust.Approve(projectDir, capability, nc.name, fingerprint, "command-line"); err != nil {
				warns = append(warns, fmt.Errorf("approve project-local %s MCP server %q: %w", transport, nc.name, err))
				continue
			}
		}
		status, err := projecttrust.Evaluate(projectDir, capability, nc.name, fingerprint)
		if err != nil {
			warns = append(warns, fmt.Errorf("evaluate project-local %s MCP server %q trust: %w", transport, nc.name, err))
			continue
		}
		if status.State != projecttrust.StateAllowed {
			warns = append(warns, fmt.Errorf("project-local %s MCP server %q is %s - skipping; pass --trust-project-mcp to approve its current configuration", transport, nc.name, status.State))
			continue
		}

		if len(nc.cfg.AutoApprove) > 0 {
			policyFingerprint, err := mcpPolicyFingerprint(nc.name, nc.cfg.AutoApprove)
			if err != nil {
				warns = append(warns, fmt.Errorf("fingerprint MCP approval policy for %q: %w", nc.name, err))
				nc.cfg.AutoApprove = nil
			} else {
				if trustProject {
					if err := projecttrust.Approve(projectDir, projecttrust.CapabilityMCPPolicy, nc.name, policyFingerprint, "command-line"); err != nil {
						warns = append(warns, fmt.Errorf("approve MCP policy for %q: %w", nc.name, err))
						nc.cfg.AutoApprove = nil
					}
				}
				policyStatus, err := projecttrust.Evaluate(projectDir, projecttrust.CapabilityMCPPolicy, nc.name, policyFingerprint)
				if err != nil {
					warns = append(warns, fmt.Errorf("evaluate MCP approval policy for %q: %w", nc.name, err))
					nc.cfg.AutoApprove = nil
				} else if policyStatus.State != projecttrust.StateAllowed {
					warns = append(warns, fmt.Errorf("project-local MCP approval policy for %q is %s - autoApprove disabled; pass --trust-project-mcp to approve its current configuration", nc.name, policyStatus.State))
					nc.cfg.AutoApprove = nil
				}
			}
		}
		out = append(out, nc)
	}
	return out, warns
}

func mcpTransportFingerprint(cfg ServerConfig) (string, error) {
	cfg.AutoApprove = nil
	return projecttrust.Fingerprint(cfg)
}

func mcpPolicyFingerprint(server string, autoApprove []string) (string, error) {
	return projecttrust.Fingerprint(struct {
		Server      string   `json:"server"`
		AutoApprove []string `json:"autoApprove"`
	}{Server: server, AutoApprove: autoApprove})
}
