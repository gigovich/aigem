package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/gigovich/aigem/internal/search"
)

const searchUsage = `usage:
  aigem search status
  aigem search set brave [--api-key KEY | --api-key-stdin]
  aigem search set browser [--engine duckduckgo|google|bing] [--profile-dir DIR] [--executable PATH]
                           [--test-host HOST]...
  aigem search clear

Configure the web-search provider the agent uses to look up current
information (package versions, latest docs, recent releases). Prefer
--api-key-stdin over --api-key; the latter may be saved in shell history.

The browser provider uses Chrome DevTools automation: it opens search results and
result pages in an isolated Chrome/Chromium profile, then extracts rendered page
text from that browser. If --profile-dir is omitted, aigem creates one in its
private state dir.

The browser_action tool lets a tester bot drive the page (login, viewport,
keyboard, DOM checks). --test-host allowlists the internal app hosts it may reach
(repeat for several). The bot fills login credentials directly, reading them from
a local file or the ticket.`

// runSearchCommand handles "aigem search ..." subcommands.
func runSearchCommand(args []string) error {
	if len(args) == 0 {
		return searchStatus()
	}
	switch args[0] {
	case "status":
		return searchStatus()
	case "set":
		return searchSet(args[1:])
	case "clear":
		if err := search.Clear(); err != nil {
			return err
		}
		fmt.Println("cleared search configuration")
		return nil
	case "-h", "--help", "help":
		fmt.Println(searchUsage)
		return nil
	default:
		return fmt.Errorf("unknown search subcommand %q\n\n%s", args[0], searchUsage)
	}
}

func searchStatus() error {
	c, err := search.Load()
	if err != nil {
		return err
	}
	fmt.Printf("search provider: %s\n", c.Describe())
	return nil
}

func searchSet(args []string) error {
	var provider, apiKey string
	var apiKeyStdin bool
	browserCfg := search.BrowserConfig{Engine: search.BrowserEngineDuckDuckGo, Mode: search.BrowserModeInteractive}
	// Seed the interactive-tester fields from any saved config so an unrelated
	// `set browser` re-run does not silently wipe test hosts.
	if existing, err := search.Load(); err == nil && existing.Browser != nil {
		browserCfg.TestHosts = existing.Browser.TestHosts
	}
	var testHosts []string
	var testHostsSet bool
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case a == "--api-key":
			if i+1 >= len(args) {
				return fmt.Errorf("--api-key needs a value")
			}
			apiKey, i = args[i+1], i+2
		case strings.HasPrefix(a, "--api-key="):
			apiKey, i = strings.TrimPrefix(a, "--api-key="), i+1
		case a == "--api-key-stdin":
			apiKeyStdin, i = true, i+1
		case a == "--engine":
			if i+1 >= len(args) {
				return fmt.Errorf("--engine needs a value")
			}
			browserCfg.Engine, i = args[i+1], i+2
		case strings.HasPrefix(a, "--engine="):
			browserCfg.Engine, i = strings.TrimPrefix(a, "--engine="), i+1
		case a == "--executable":
			if i+1 >= len(args) {
				return fmt.Errorf("--executable needs a value")
			}
			browserCfg.Executable, i = args[i+1], i+2
		case strings.HasPrefix(a, "--executable="):
			browserCfg.Executable, i = strings.TrimPrefix(a, "--executable="), i+1
		case a == "--profile-dir":
			if i+1 >= len(args) {
				return fmt.Errorf("--profile-dir needs a value")
			}
			browserCfg.ProfileDir, i = args[i+1], i+2
		case strings.HasPrefix(a, "--profile-dir="):
			browserCfg.ProfileDir, i = strings.TrimPrefix(a, "--profile-dir="), i+1
		case a == "--test-host":
			if i+1 >= len(args) {
				return fmt.Errorf("--test-host needs a value")
			}
			testHosts, testHostsSet, i = append(testHosts, args[i+1]), true, i+2
		case strings.HasPrefix(a, "--test-host="):
			testHosts, testHostsSet, i = append(testHosts, strings.TrimPrefix(a, "--test-host=")), true, i+1
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, searchUsage)
		default:
			if provider != "" {
				return fmt.Errorf("unexpected argument %q\n\n%s", a, searchUsage)
			}
			provider, i = a, i+1
		}
	}
	if provider == "" {
		return fmt.Errorf("provider is required\n\n%s", searchUsage)
	}
	if apiKey != "" && apiKeyStdin {
		return fmt.Errorf("use only one of --api-key and --api-key-stdin")
	}
	if provider != search.ProviderBrowser && testHostsSet {
		return fmt.Errorf("--test-host applies only to the browser provider")
	}
	if apiKeyStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read api key from stdin: %w", err)
		}
		apiKey = strings.TrimSpace(string(data))
	}
	if testHostsSet {
		browserCfg.TestHosts = testHosts
	}

	switch provider {
	case search.ProviderBrave:
		if apiKey == "" {
			return fmt.Errorf("brave needs an API key (pass --api-key-stdin or --api-key)")
		}
		c := search.Config{Provider: search.ProviderBrave, Brave: &search.BraveConfig{APIKey: apiKey}}
		if err := search.Save(c); err != nil {
			return err
		}
		fmt.Println("saved brave search configuration")
		return nil
	case search.ProviderBrowser:
		prepared, err := search.PrepareBrowserConfig(&browserCfg)
		if err != nil {
			return err
		}
		cfg := search.Config{Provider: search.ProviderBrowser, Browser: &prepared}
		if _, err := cfg.Searcher(); err != nil {
			return err
		}
		if err := search.Save(cfg); err != nil {
			return err
		}
		fmt.Println("saved browser search configuration")
		return nil
	default:
		return fmt.Errorf("unknown provider %q (supported: brave, browser)", provider)
	}
}

// runSearchSetup is the first-run wizard. It prints a menu, collects the
// provider (and its key), verifies it, and persists the config. A blank choice
// skips setup, leaving the agent without web_search.
func runSearchSetup() error {
	// Only prompt on a real terminal. Under a pipe/redirect the reads here would
	// steal the agent's stdin; skip setup and let the user run `aigem search` later.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	fmt.Println()
	fmt.Println("Welcome to aigem. Set up the agent's web-search provider.")
	fmt.Println("This lets the agent look up current info (package versions, latest docs)")
	fmt.Println("instead of relying on its training data.")
	fmt.Println()
	fmt.Println("  1) Brave Search (API key)")
	fmt.Println("  2) Local browser (automated Chrome; opens DuckDuckGo results)")
	fmt.Println("  Enter to skip")
	fmt.Println()
	fmt.Print("Choose a provider [1]: ")

	choice, err := readLine(os.Stdin)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(choice) {
	case "":
		fmt.Println("Skipped. Run `aigem search set brave --api-key-stdin` or `aigem search set browser` later to enable it.")
		return nil
	case "1", "brave":
		return setupBrave()
	case "2", "browser":
		return setupBrowser()
	default:
		fmt.Println("Unrecognized choice. Skipping search setup.")
		return nil
	}
}

// setupBrave prompts for the Brave key, verifies it with a probe query, and saves.
func setupBrave() error {
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Print("Brave Search API key (input hidden): ")
		key, err := readSecret(os.Stdin)
		if err != nil {
			return err
		}
		key = strings.TrimSpace(key)
		if key == "" {
			fmt.Println("No key entered. Skipping search setup.")
			return nil
		}
		fmt.Print("Verifying key... ")
		if err := verifyBrave(key); err != nil {
			fmt.Println("failed.")
			fmt.Printf("  %v\n", err)
			if attempt < 2 {
				fmt.Println("  Try again.")
				continue
			}
			fmt.Println("Saving anyway; fix it later with `aigem search set brave --api-key-stdin`.")
		} else {
			fmt.Println("ok.")
		}
		c := search.Config{Provider: search.ProviderBrave, Brave: &search.BraveConfig{APIKey: key}}
		if err := search.Save(c); err != nil {
			return err
		}
		fmt.Println("Saved. The agent can now use web_search.")
		fmt.Println()
		return nil
	}
	return nil
}

func setupBrowser() error {
	browserCfg, err := search.PrepareBrowserConfig(&search.BrowserConfig{
		Engine: search.BrowserEngineDuckDuckGo,
		Mode:   search.BrowserModeInteractive,
	})
	if err != nil {
		return err
	}
	cfg := search.Config{Provider: search.ProviderBrowser, Browser: &browserCfg}
	if err := search.Save(cfg); err != nil {
		return err
	}
	fmt.Println("Saved. web_search will open DuckDuckGo results and result pages in your local browser profile.")
	fmt.Println("Complete any browser profile prompts now; later searches will use Chrome automation.")
	fmt.Println()
	return nil
}

// verifyBrave runs a cheap probe query to confirm the key works.
func verifyBrave(key string) error {
	c := search.Config{Provider: search.ProviderBrave, Brave: &search.BraveConfig{APIKey: key}}
	s, err := c.Searcher()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = s.Search(ctx, "hello", 1)
	return err
}

// readLine reads a single line from f without buffering ahead, so a subsequent
// readSecret on the same fd does not lose bytes.
func readLine(f *os.File) (string, error) {
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			b = append(b, buf[0])
		}
		if err != nil {
			// EOF (Ctrl+D) ends the line; an empty line then reads as a skip rather
			// than a fatal error. Other errors propagate.
			if err == io.EOF {
				break
			}
			return string(b), err
		}
	}
	return strings.TrimRight(string(b), "\r"), nil
}

// readSecret reads a line with terminal echo disabled when stdin is a terminal,
// falling back to a plain line read otherwise.
func readSecret(f *os.File) (string, error) {
	if term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return readLine(f)
}
