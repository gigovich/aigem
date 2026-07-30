package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gigovich/aigem/internal/local"

	"golang.org/x/term"
)

const modelsUsage = `usage:
  aigem models                 list providers/models (* needs login)
  aigem models init            set up the local llama.cpp model and start it
  aigem models status          show local model setup and server state
  aigem models start           start the local llama-server
  aigem models stop            stop the local llama-server
  aigem models reset           stop the server and clear local setup`

// runModelsCommand dispatches "aigem models ..." subcommands.
func runModelsCommand(args []string) error {
	if len(args) == 0 {
		return listModels()
	}
	switch args[0] {
	case "init":
		return modelsInit()
	case "status":
		return modelsStatus()
	case "start":
		return modelsStart()
	case "stop":
		return modelsStop()
	case "reset":
		return modelsReset()
	case "-h", "--help", "help":
		fmt.Println(modelsUsage)
		return nil
	default:
		return fmt.Errorf("unknown models subcommand %q\n\n%s", args[0], modelsUsage)
	}
}

// runLocalInitWizard interactively builds and saves a local.Config. Shared by
// `aigem models init` and the CLI startup flow.
func runLocalInitWizard() (local.Config, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return local.Config{}, fmt.Errorf("local model setup needs an interactive terminal; run `aigem models init`")
	}
	cfg := local.Defaults()
	fmt.Println()
	fmt.Println("Set up the local llama.cpp model.")
	fmt.Println("  1) Hugging Face download (-hf)  [default]")
	fmt.Println("  2) Local .gguf file path")
	fmt.Print("Model source [1]: ")
	choice, err := readLine(os.Stdin)
	if err != nil {
		return local.Config{}, err
	}
	switch strings.TrimSpace(choice) {
	case "", "1", "hf":
		cfg.SourceKind = local.SourceHF
		fmt.Printf("HF repo:quant [%s]: ", cfg.HFRepo)
		repo, err := readLine(os.Stdin)
		if err != nil {
			return local.Config{}, err
		}
		if r := strings.TrimSpace(repo); r != "" {
			cfg.HFRepo = r
		}
	case "2", "path":
		cfg.SourceKind = local.SourcePath
		fmt.Print("Path to .gguf file: ")
		p, err := readLine(os.Stdin)
		if err != nil {
			return local.Config{}, err
		}
		cfg.ModelPath = strings.TrimSpace(p)
		if cfg.ModelPath != "" {
			cfg.ModelName = filepath.Base(cfg.ModelPath)
		}
	default:
		return local.Config{}, fmt.Errorf("unrecognized choice %q", strings.TrimSpace(choice))
	}
	fmt.Printf("llama-server binary [%s]: ", cfg.BinaryPath)
	bin, err := readLine(os.Stdin)
	if err != nil {
		return local.Config{}, err
	}
	if b := strings.TrimSpace(bin); b != "" {
		cfg.BinaryPath = b
	}
	if err := cfg.Validate(); err != nil {
		return local.Config{}, err
	}
	if err := local.Save(cfg); err != nil {
		return local.Config{}, err
	}
	fmt.Printf("Saved. Command:\n  %s\n", cfg.CommandString())
	return cfg, nil
}

func modelsInit() error {
	cfg, err := runLocalInitWizard()
	if err != nil {
		return err
	}
	fmt.Print("Start the server now? [Y/n]: ")
	line, _ := readLine(os.Stdin)
	if l := strings.TrimSpace(strings.ToLower(line)); l != "" && l != "y" && l != "yes" {
		fmt.Println("Skipped. Run `aigem models start` when ready.")
		return nil
	}
	return startLocalServerCLI(cfg)
}

// cliProgressLine renders a launch-progress line that names the model being
// downloaded, with a percentage when the size is known.
func cliProgressLine(name string, p local.Progress) string {
	switch p.Phase {
	case local.PhaseReady:
		return name + ": ready"
	case local.PhaseLoading:
		return "downloading " + name + ": loading into memory..."
	default:
		if f := p.Fraction(); f >= 0 {
			return fmt.Sprintf("downloading %s: %.0f%%  %s", name, f*100, p.Detail())
		}
		return fmt.Sprintf("downloading %s: %s", name, p.Detail())
	}
}

// startLocalServerCLI starts the daemon, streaming download/load progress to
// stderr (\033[K clears any leftover from a longer previous line), and prints a
// final status line. Shared by `aigem models init/start` and -p startup so the
// three paths behave identically.
func startLocalServerCLI(cfg local.Config) error {
	fmt.Fprintln(os.Stderr, "starting llama-server (first run downloads the model)...")
	err := local.Start(context.Background(), cfg, func(p local.Progress) {
		fmt.Fprintf(os.Stderr, "\r\033[K%s", cliProgressLine(cfg.ModelName, p))
	})
	fmt.Fprintln(os.Stderr) // terminate the \r progress line
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "llama-server is up at", cfg.BaseURL())
	return nil
}

func modelsStatus() error {
	cfg, _, err := local.Load()
	if err != nil {
		return err
	}
	r := local.Status(cfg)
	fmt.Printf("initialized: %t\nrunning:     %t (pid %d)\nreachable:   %t\nurl:         %s\ncommand:     %s\nlog:         %s\n",
		r.Initialized, r.Running, r.PID, r.Reachable, cfg.BaseURL(), r.Command, r.LogPath)
	return nil
}

func modelsStart() error {
	cfg, exists, err := local.Load()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("local model not initialized; run `aigem models init`")
	}
	return startLocalServerCLI(cfg)
}

func modelsStop() error {
	if err := local.Stop(); err != nil {
		return err
	}
	fmt.Println("stopped llama-server")
	return nil
}

func modelsReset() error {
	fmt.Print("Stop the server and clear local setup? [y/N]: ")
	line, _ := readLine(os.Stdin)
	if l := strings.TrimSpace(strings.ToLower(line)); l != "y" && l != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}
	if err := local.Reset(); err != nil {
		return err
	}
	fmt.Println("cleared local setup")
	return nil
}
