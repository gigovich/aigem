package hooks

import "testing"

// A wrong flag fails silently: cmd.exe rejects "-c" and runs nothing, so a hook
// that looked configured would simply never fire.
func TestShellFlag(t *testing.T) {
	cases := []struct {
		shell string
		want  string
	}{
		{"bash", "-c"},
		{"sh", "-c"},
		{"zsh", "-c"},
		{"/usr/bin/bash", "-c"},
		{"", "-c"}, // callers only pass a non-empty shell, but never panic on it

		{"cmd", "/c"},
		{"cmd.exe", "/c"},
		{"CMD.EXE", "/c"},
		{`C:\Windows\System32\cmd.exe`, "/c"},

		{"powershell", "-Command"},
		{"pwsh", "-Command"},
		{"PowerShell.exe", "-Command"},
	}
	for _, tc := range cases {
		if got := shellFlag(tc.shell); got != tc.want {
			t.Errorf("shellFlag(%q) = %q, want %q", tc.shell, got, tc.want)
		}
	}
}
