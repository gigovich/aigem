package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gigovich/aigem/internal/mcp"
)

func TestUpsertPreservesOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"a":1},"model":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mcp.ServerConfig{Command: "npx", Args: []string{"-y", "pkg"}}
	if err := upsertServer(path, "fs", cfg); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["hooks"]; !ok {
		t.Error("hooks key was dropped")
	}
	if doc["model"] != "x" {
		t.Error("model key was dropped")
	}
	servers := doc["mcpServers"].(map[string]any)
	fs := servers["fs"].(map[string]any)
	if fs["command"] != "npx" {
		t.Fatalf("command = %v", fs["command"])
	}
	if _, ok := fs["url"]; ok {
		t.Error("empty url should be omitted")
	}
}

func TestDeleteServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	_ = upsertServer(path, "a", mcp.ServerConfig{URL: "http://x"})
	_ = upsertServer(path, "b", mcp.ServerConfig{URL: "http://y"})

	removed, err := deleteServer(path, "a")
	if err != nil || !removed {
		t.Fatalf("delete a: removed=%v err=%v", removed, err)
	}
	if removed, _ := deleteServer(path, "missing"); removed {
		t.Error("deleting a missing server should report false")
	}
	doc, _ := readDoc(path)
	servers := docServers(doc)
	if len(servers) != 1 {
		t.Fatalf("want 1 server left, got %d", len(servers))
	}
	if _, ok := servers["b"]; !ok {
		t.Errorf("server b should remain: %+v", servers)
	}
}

func TestBuildServerConfigValidation(t *testing.T) {
	if _, err := buildServerConfig("", nil, nil, nil); err == nil {
		t.Error("no transport should error")
	}
	if _, err := buildServerConfig("http://x", []string{"cmd"}, nil, nil); err == nil {
		t.Error("both transports should error")
	}
	cfg, err := buildServerConfig("", []string{"npx", "pkg"}, nil, []string{"K=V"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "npx" || cfg.Env["K"] != "V" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := buildServerConfig("http://x", nil, []string{"bad-header"}, nil); err == nil {
		t.Error("malformed header should error")
	}
}
