package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gigovich/aigem/internal/pathgrant"
)

const pathsUsage = `usage:
  aigem paths [list]              directories this project may read outside its working directory
  aigem paths list --all          every project's grants
  aigem paths forget <dir>        drop one grant
  aigem paths forget --all        drop every grant made from this project

A grant is created from the confirmation box when the agent asks for a file
outside the working directory and you answer "Always (this folder)". It covers
reads only; a write outside the working directory is always asked about.`

func runPathsCommand(args []string) error {
	cmd := "list"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "list":
		return pathsList(args)
	case "forget":
		return pathsForget(args)
	case "help", "-h", "--help":
		fmt.Println(pathsUsage)
		return nil
	}
	return fmt.Errorf("unknown paths command %q\n\n%s", cmd, pathsUsage)
}

func pathsList(args []string) error {
	if len(args) > 0 && args[0] == "--all" {
		grants, err := pathgrant.ListAll()
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			fmt.Println("no path grants recorded")
			return nil
		}
		for _, g := range grants {
			fmt.Printf("%s\n  → %s  (%s)\n", g.Project, g.Dir, g.GrantedAt.Local().Format("2006-01-02 15:04"))
		}
		return nil
	}
	project, err := currentProject()
	if err != nil {
		return err
	}
	grants, err := pathgrant.List(project)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		fmt.Printf("no path grants for %s\n", project)
		return nil
	}
	fmt.Printf("%s may also read:\n", project)
	for _, g := range grants {
		fmt.Printf("  %s  (%s)\n", g.Dir, g.GrantedAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

func pathsForget(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("paths forget needs a directory or --all\n\n%s", pathsUsage)
	}
	project, err := currentProject()
	if err != nil {
		return err
	}
	if args[0] == "--all" {
		n, err := pathgrant.ForgetProject(project)
		if err != nil {
			return err
		}
		fmt.Printf("dropped %d grant(s) for %s\n", n, project)
		return nil
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	found, err := pathgrant.Forget(project, dir)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no grant for %s in %s (see: aigem paths list)", dir, project)
	}
	fmt.Printf("dropped %s\n", dir)
	return nil
}

// currentProject returns the sandbox root a session started here would use, so
// the grants listed are exactly the ones that session would honor. It must
// canonicalize the same way tools.NewRegistry does - grants are keyed by that
// root, and a git root or an unresolved symlink would key a different set.
func currentProject() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}
