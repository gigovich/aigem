package main

import (
	"os"
	"testing"

	"github.com/gigovich/aigem/internal/search"
)

// readLineFrom writes s to a pipe (no newline appended) and reads it back, so we
// exercise readLine's EOF/newline handling deterministically.
func readLineFrom(t *testing.T, s string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	go func() {
		_, _ = w.WriteString(s)
		w.Close()
	}()
	return readLine(r)
}

func TestSearchSetBrowserSavesConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := runSearchCommand([]string{"set", "browser"}); err != nil {
		t.Fatalf("runSearchCommand: %v", err)
	}
	cfg, err := search.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider != search.ProviderBrowser || cfg.Browser == nil {
		t.Fatalf("expected browser config, got %+v", cfg)
	}
	if cfg.Browser.Engine != search.BrowserEngineDuckDuckGo || cfg.Browser.Executable != "" || cfg.Browser.ProfileDir == "" {
		t.Fatalf("unexpected browser config: %+v", cfg.Browser)
	}
}

func TestReadLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newline terminated", "1\n", "1"},
		{"eof no newline", "brave", "brave"},
		{"empty then eof", "", ""},
		{"crlf trimmed", "2\r\n", "2"},
		{"value then more buffered", "1\nsecret\n", "1"},
	}
	for _, tc := range cases {
		got, err := readLineFrom(t, tc.in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: readLine=%q want %q", tc.name, got, tc.want)
		}
	}
}
