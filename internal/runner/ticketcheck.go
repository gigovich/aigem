package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/tools"
)

// checkBudget bounds a repository's check. A variable so a test can shrink it.
var checkBudget = 15 * time.Minute

// checkOutput is how much of a failed check's output a ticket comment keeps.
const checkOutput = 4 << 10

// readCheck reads the check a repository declares in .aigem/project.json; "" when none.
func readCheck(repo string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repo, ".aigem", "project.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Check string `json:"check"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf(".aigem/project.json: %w", err)
	}
	return strings.TrimSpace(cfg.Check), nil
}

// runCheck runs check through the shell in dir within checkBudget, killing its whole process
// group when the budget or ctx ends, and returns the end of its output.
func runCheck(ctx context.Context, dir, check string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	out := gitx.NewTail(checkOutput)
	cmd := exec.CommandContext(ctx, "sh", "-c", check)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	tools.ConfigureProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("it ran past %s", checkBudget)
	}
	return out.String(), err
}
