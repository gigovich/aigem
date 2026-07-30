package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/mcp"
)

const mcpUsage = `usage:
  aigem mcp list
  aigem mcp add [--global] [--env K=V]... <name> -- <command> [args...]
  aigem mcp add [--global] [--oauth] [--header "K: V"]... <name> --url <url>
  aigem mcp remove [--global] <name>
  aigem mcp login <name>     authorize an --oauth http server in the browser
  aigem mcp logout <name>    discard a server's stored OAuth token

By default servers are written to the project's .mcp.json; --global writes to
~/.config/aigem/settings.json.`

// runMCPCommand handles "aigem mcp ..." subcommands. It returns an error to be
// fatal-printed by the caller.
func runMCPCommand(args []string) error {
	if len(args) == 0 {
		fmt.Println(mcpUsage)
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		return mcpList(cwd)
	case "add":
		return mcpAdd(cwd, args[1:])
	case "remove", "rm":
		return mcpRemove(cwd, args[1:])
	case "login":
		return mcpLogin(cwd, args[1:])
	case "logout":
		return mcpLogout(args[1:])
	case "-h", "--help", "help":
		fmt.Println(mcpUsage)
		return nil
	default:
		return fmt.Errorf("unknown mcp subcommand %q\n\n%s", args[0], mcpUsage)
	}
}

func mcpList(cwd string) error {
	servers, warns := mcp.ListConfigured(cwd)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if len(servers) == 0 {
		fmt.Println("no MCP servers configured")
		return nil
	}
	for _, s := range servers {
		kind, detail := s.Cfg.Transport()
		fmt.Printf("%-16s %-6s %s\n", s.Name, kind, detail)
	}
	return nil
}

func mcpAdd(cwd string, args []string) error {
	var (
		global  bool
		oauth   bool
		url     string
		name    string
		headers []string
		envs    []string
		command []string
	)
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case a == "--":
			command = args[i+1:]
			i = len(args)
		case a == "--global":
			global = true
			i++
		case a == "--oauth":
			oauth = true
			i++
		case a == "--url":
			if i+1 >= len(args) {
				return fmt.Errorf("--url needs a value")
			}
			url, i = args[i+1], i+2
		case strings.HasPrefix(a, "--url="):
			url, i = strings.TrimPrefix(a, "--url="), i+1
		case a == "--header":
			if i+1 >= len(args) {
				return fmt.Errorf("--header needs a value")
			}
			headers, i = append(headers, args[i+1]), i+2
		case a == "--env":
			if i+1 >= len(args) {
				return fmt.Errorf("--env needs a value")
			}
			envs, i = append(envs, args[i+1]), i+2
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, mcpUsage)
		default:
			if name != "" {
				return fmt.Errorf("unexpected argument %q (put the server command after --)\n\n%s", a, mcpUsage)
			}
			name, i = a, i+1
		}
	}

	if name == "" {
		return fmt.Errorf("server name is required\n\n%s", mcpUsage)
	}
	cfg, err := buildServerConfig(url, command, headers, envs)
	if err != nil {
		return err
	}
	if oauth {
		if cfg.URL == "" {
			return fmt.Errorf("--oauth applies to --url (http) servers only")
		}
		cfg.OAuth = true
	}

	path, err := targetFile(cwd, global)
	if err != nil {
		return err
	}
	if err := upsertServer(path, name, cfg); err != nil {
		return err
	}
	fmt.Printf("added MCP server %q to %s\n", name, path)
	return nil
}

func mcpRemove(cwd string, args []string) error {
	var global bool
	var name string
	for _, a := range args {
		switch {
		case a == "--global":
			global = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, mcpUsage)
		default:
			name = a
		}
	}
	if name == "" {
		return fmt.Errorf("server name is required\n\n%s", mcpUsage)
	}
	path, err := targetFile(cwd, global)
	if err != nil {
		return err
	}
	removed, err := deleteServer(path, name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("server %q not found in %s", name, path)
	}
	fmt.Printf("removed MCP server %q from %s\n", name, path)
	return nil
}

func mcpLogin(cwd string, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: aigem mcp login <name>")
	}
	if err := mcp.Login(context.Background(), cwd, version, args[0]); err != nil {
		return err
	}
	fmt.Printf("authorized MCP server %q\n", args[0])
	return nil
}

func mcpLogout(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: aigem mcp logout <name>")
	}
	if err := mcp.Logout(args[0]); err != nil {
		return err
	}
	fmt.Printf("cleared stored OAuth token for %q\n", args[0])
	return nil
}

// buildServerConfig assembles a ServerConfig from the parsed flags, validating
// that exactly one transport is specified.
func buildServerConfig(url string, command, headers, envs []string) (mcp.ServerConfig, error) {
	if url != "" && len(command) > 0 {
		return mcp.ServerConfig{}, fmt.Errorf("specify either --url or a -- command, not both")
	}
	if url == "" && len(command) == 0 {
		return mcp.ServerConfig{}, fmt.Errorf("give a command after -- (stdio) or --url (http)")
	}
	cfg := mcp.ServerConfig{}
	if url != "" {
		cfg.URL = url
		if len(headers) > 0 {
			cfg.Headers = map[string]string{}
			for _, h := range headers {
				k, v, ok := strings.Cut(h, ":")
				if !ok {
					return cfg, fmt.Errorf("bad --header %q, want \"Key: value\"", h)
				}
				cfg.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		return cfg, nil
	}
	cfg.Command = command[0]
	cfg.Args = command[1:]
	if len(envs) > 0 {
		cfg.Env = map[string]string{}
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if !ok {
				return cfg, fmt.Errorf("bad --env %q, want KEY=value", e)
			}
			cfg.Env[k] = v
		}
	}
	return cfg, nil
}

func targetFile(cwd string, global bool) (string, error) {
	if global {
		return config.GlobalSettingsFile()
	}
	return config.ProjectMCPFile(cwd), nil
}

// upsertServer reads path (JSON, possibly absent), sets mcpServers[name], and
// writes it back, preserving any other top-level keys.
func upsertServer(path, name string, cfg mcp.ServerConfig) error {
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	servers := docServers(doc)
	var cfgMap map[string]any
	raw, _ := json.Marshal(cfg)
	_ = json.Unmarshal(raw, &cfgMap)
	servers[name] = cfgMap
	doc["mcpServers"] = servers
	return writeDoc(path, doc)
}

func deleteServer(path, name string) (bool, error) {
	doc, err := readDoc(path)
	if err != nil {
		return false, err
	}
	servers := docServers(doc)
	if _, ok := servers[name]; !ok {
		return false, nil
	}
	delete(servers, name)
	doc["mcpServers"] = servers
	return true, writeDoc(path, doc)
}

func readDoc(path string) (map[string]any, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return doc, nil
}

func docServers(doc map[string]any) map[string]any {
	if m, ok := doc["mcpServers"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func writeDoc(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
