package runner

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/mcp"
)

func TestReleaseProjectRuntimeBoundsMCPShutdown(t *testing.T) {
	oldTimeout := projectRuntimeCloseTimeout
	oldClose := closeProjectRuntimeMCP
	t.Cleanup(func() {
		projectRuntimeCloseTimeout = oldTimeout
		closeProjectRuntimeMCP = oldClose
	})

	projectRuntimeCloseTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	closeProjectRuntimeMCP = func(*mcp.Manager) {
		close(started)
		time.Sleep(200 * time.Millisecond)
	}

	runtime := &ProjectRuntime{Root: t.TempDir(), MCP: &mcp.Manager{}, refs: 1}
	begin := time.Now()
	releaseProjectRuntime(runtime)
	elapsed := time.Since(begin)
	select {
	case <-started:
	default:
		t.Fatal("MCP shutdown was not started")
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("release waited for a slow MCP shutdown: %s", elapsed)
	}
}

func TestReleaseProjectRuntimeClosesMCPOnlyOnce(t *testing.T) {
	oldClose := closeProjectRuntimeMCP
	t.Cleanup(func() { closeProjectRuntimeMCP = oldClose })

	var closes atomic.Int32
	closeProjectRuntimeMCP = func(*mcp.Manager) { closes.Add(1) }
	runtime := &ProjectRuntime{Root: t.TempDir(), MCP: &mcp.Manager{}, refs: 1}

	releaseProjectRuntime(runtime)
	releaseProjectRuntime(runtime)
	if got := closes.Load(); got != 1 {
		t.Fatalf("MCP shutdown count = %d, want 1", got)
	}
}
