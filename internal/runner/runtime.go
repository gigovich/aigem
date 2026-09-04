package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/search"
)

// ProjectRuntime is the process-wide part of a project's environment. It is
// reference counted so multiple roots/worktrees can share one MCP manager
// without allowing one session to close it underneath another.
type ProjectRuntime struct {
	Root    string
	Agents  *agent.SubagentRegistry
	MCP     *mcp.Manager
	Search  search.Config
	Version string

	refs  int
	close sync.Once
}

var projectRuntimeCloseTimeout = 5 * time.Second

var closeProjectRuntimeMCP = func(manager *mcp.Manager) {
	manager.Close()
}

var projectRuntimes = struct {
	sync.Mutex
	items map[string]*ProjectRuntime
}{items: make(map[string]*ProjectRuntime)}

// acquireProjectRuntime returns the runtime for root, creating it on first use.
// The root, rather than cwd, is the cache key: worktrees and subdirectories of
// one project must not start duplicate project-level MCP servers.
func acquireProjectRuntime(ctx context.Context, root, version string, searchCfg search.Config, trustMCP bool) (*ProjectRuntime, []error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	projectRuntimes.Lock()
	defer projectRuntimes.Unlock()
	if runtime := projectRuntimes.items[root]; runtime != nil {
		runtime.refs++
		return runtime, nil, nil
	}

	runtime := &ProjectRuntime{
		Root:    root,
		Agents:  agent.DefaultSubagents(),
		Search:  searchCfg,
		Version: version,
		refs:    1,
	}
	var warnings []error
	if dir, err := config.AgentsDir(); err == nil {
		if info, statErr := os.Stat(dir); statErr == nil && !info.IsDir() {
			warnings = append(warnings, fmt.Errorf("could not load custom agents: %s is not a directory", dir))
		} else if err := agent.LoadSubagentsInto(runtime.Agents, dir); err != nil {
			warnings = append(warnings, fmt.Errorf("could not load custom agents: %w", err))
		}
	}
	manager, mcpWarnings := mcp.NewWithTrust(root, version, trustMCP)
	runtime.MCP = manager
	warnings = append(warnings, mcpWarnings...)
	if !manager.Empty() {
		manager.Connect(ctx)
		if err := ctx.Err(); err != nil {
			manager.Close()
			return nil, warnings, err
		}
		for _, warning := range manager.Warnings() {
			warnings = append(warnings, errors.New(warning))
		}
	}
	if err := ctx.Err(); err != nil {
		manager.Close()
		return nil, warnings, err
	}
	projectRuntimes.items[root] = runtime
	return runtime, warnings, nil
}

// releaseProjectRuntime drops one environment's reference. The last release
// removes the cache entry before closing MCP, so a concurrently acquired
// environment cannot observe a manager during shutdown.
func releaseProjectRuntime(runtime *ProjectRuntime) {
	if runtime == nil {
		return
	}
	projectRuntimes.Lock()
	if runtime.refs > 0 {
		runtime.refs--
	}
	if runtime.refs != 0 {
		projectRuntimes.Unlock()
		return
	}
	if projectRuntimes.items[runtime.Root] == runtime {
		delete(projectRuntimes.items, runtime.Root)
	}
	projectRuntimes.Unlock()
	if runtime.MCP == nil {
		return
	}
	runtime.close.Do(func() {
		closed := make(chan struct{})
		go func() {
			closeProjectRuntimeMCP(runtime.MCP)
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(projectRuntimeCloseTimeout):
			// The manager is no longer reachable from the cache. A slow SDK shutdown
			// may finish in its own goroutine, but it cannot hold up daemon shutdown.
		}
	})
}
