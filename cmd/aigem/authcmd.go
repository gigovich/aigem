package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
)

const authUsage = `usage:
  aigem auth login <provider> [--api-key KEY | --api-key-stdin]
  aigem auth logout <provider>
  aigem auth status

Providers: openai (ChatGPT subscription via browser, or API key for the
OpenAI API) and xai (Grok subscription - SuperGrok / X Premium+ - via a
device code approved in any browser, or API key for the xAI API). Without an
API-key option, "login openai" opens the ChatGPT OAuth flow and "login xai"
starts the Grok device-code flow. Prefer --api-key-stdin or $OPENAI_API_KEY /
$XAI_API_KEY; --api-key may be saved in shell history or process listings.`

// runAuthCommand handles "aigem auth ..." subcommands.
func runAuthCommand(args []string) error {
	if len(args) == 0 {
		fmt.Println(authUsage)
		return nil
	}
	switch args[0] {
	case "login":
		return authLogin(args[1:])
	case "logout":
		return authLogout(args[1:])
	case "status":
		return authStatus()
	case "-h", "--help", "help":
		fmt.Println(authUsage)
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand %q\n\n%s", args[0], authUsage)
	}
}

func authLogin(args []string) error {
	var provider, apiKey string
	var apiKeyStdin bool
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case a == "--api-key":
			if i+1 >= len(args) {
				return errors.New("--api-key needs a value")
			}
			apiKey, i = args[i+1], i+2
		case strings.HasPrefix(a, "--api-key="):
			apiKey, i = strings.TrimPrefix(a, "--api-key="), i+1
		case a == "--api-key-stdin":
			apiKeyStdin, i = true, i+1
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, authUsage)
		default:
			if provider != "" {
				return fmt.Errorf("unexpected argument %q\n\n%s", a, authUsage)
			}
			provider, i = a, i+1
		}
	}
	if provider == "" {
		return fmt.Errorf("provider is required\n\n%s", authUsage)
	}
	if apiKey != "" && apiKeyStdin {
		return errors.New("use only one of --api-key and --api-key-stdin")
	}
	if apiKeyStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read api key from stdin: %w", err)
		}
		apiKey = strings.TrimSpace(string(data))
		if apiKey == "" {
			return errors.New("empty API key on stdin")
		}
	}
	if apiKey != "" {
		if err := auth.Put(provider, auth.Record{Kind: auth.KindAPIKey, Key: apiKey}); err != nil {
			return err
		}
		auth.ResetSources()
		fmt.Printf("stored API key for %q\n", provider)
		return nil
	}
	switch provider {
	case llm.OpenAIProviderID:
		rec, err := auth.LoginChatGPT(context.Background(), true)
		if err != nil {
			return err
		}
		if err := auth.Put(provider, rec); err != nil {
			return err
		}
		auth.ResetSources()
		fmt.Printf("logged in to ChatGPT for %q\n", provider)
		return nil
	case llm.XAIProviderID:
		rec, err := auth.LoginXAIDevice(context.Background())
		if err != nil {
			return err
		}
		if err := auth.Put(provider, rec); err != nil {
			return err
		}
		auth.ResetSources()
		fmt.Printf("logged in to Grok subscription for %q\n", provider)
		return nil
	}
	return fmt.Errorf("provider %q supports --api-key login only", provider)
}

func authLogout(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: aigem auth logout <provider>")
	}
	if err := auth.Delete(args[0]); err != nil {
		return err
	}
	auth.ResetSources()
	fmt.Printf("cleared stored credential for %q\n", args[0])
	return nil
}

func authStatus() error {
	reg := defaultModelRegistry()
	fmt.Println("provider          auth")
	for _, p := range reg.Providers() {
		var state string
		if p.NeedsAuth() {
			if d := auth.Describe(p.ID); d != "" {
				state = d
			} else {
				state = "not logged in"
			}
		} else {
			state = "no auth needed"
		}
		fmt.Printf("%-16s  %s\n", p.ID, state)
	}
	return nil
}

// listModels lists every resolved provider/model and which are usable.
func listModels() error {
	reg := defaultModelRegistry()
	needsAuth := map[string]bool{}
	for _, p := range reg.Providers() {
		needsAuth[p.ID] = p.NeedsAuth()
	}
	models := reg.Models()
	llm.SortModelsByRef(models)
	var anyLocked bool
	for _, m := range models {
		mark := " "
		if needsAuth[m.Provider] && !auth.IsAuthenticated(m.Provider) {
			mark = "*" // needs login
			anyLocked = true
		}
		ctx := ""
		if cw := listedContextWindow(m); cw > 0 {
			ctx = fmt.Sprintf("  ctx:%d", cw)
		}
		fmt.Printf("%s %-28s %s%s\n", mark, m.Ref(), m.Name, ctx)
	}
	if anyLocked {
		fmt.Fprintln(os.Stderr, "\n* needs login (aigem auth login <provider>)")
	}
	return nil
}

// listedContextWindow returns the window a session would actually get for m,
// which on the ChatGPT subscription is narrower than the model's API-key window.
// Listing the preset instead would advertise a window the stored credential
// cannot buy.
func listedContextWindow(m llm.ModelInfo) int {
	sub := llm.SubscriptionContextWindow(m.ID)
	if sub == 0 || (m.ContextWindow != 0 && sub >= m.ContextWindow) {
		return m.ContextWindow
	}
	cred, err := auth.CredentialForModel(context.Background(), m.Provider, m.ID)
	if err != nil || cred.Kind != llm.AuthOAuthChatGPT {
		return m.ContextWindow
	}
	return sub
}

// defaultModelRegistry builds the registry with the built-in local defaults, for
// the auth/models subcommands which run before flag parsing.
func defaultModelRegistry() *llm.Registry {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	cfg, _, _ := local.Load()
	reg, warns := llm.NewRegistry(cwd, localProvider(cfg, defaultMaxTokens))
	warnModelsConfig(warns)
	return reg
}

func warnModelsConfig(warns []string) {
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "warning: models config:", w)
	}
}
