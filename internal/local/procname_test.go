package local

import "testing"

// exeName guards the Windows stop path: a pid whose image name does not match the
// configured binary is left alone, because Windows recycles pids and terminating
// the wrong process is not recoverable.
func TestExeNameMatching(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		reported   string
		want       bool
	}{
		{"bare name against reported full path", "llama-server", `C:\tools\llama-server.exe`, true},
		{"configured full path", `C:\tools\llama-server.exe`, `C:\tools\llama-server.exe`, true},
		{"case differences", "LLAMA-Server", `C:\Tools\llama-server.EXE`, true},
		{"different install location still matches", "llama-server", `D:\apps\llama-server.exe`, true},
		{"surrounding whitespace", "  llama-server  ", `C:\tools\llama-server.exe`, true},
		{"unix-style configured path", "/usr/local/bin/llama-server", "/usr/local/bin/llama-server", true},

		{"unrelated recycled pid", "llama-server", `C:\Windows\System32\notepad.exe`, false},
		{"prefix is not a match", "llama-server", `C:\tools\llama-server-proxy.exe`, false},
		{"empty reported image", "llama-server", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exeName(tc.configured) == exeName(tc.reported); got != tc.want {
				t.Errorf("exeName(%q)==exeName(%q) = %v, want %v (%q vs %q)",
					tc.configured, tc.reported, got, tc.want, exeName(tc.configured), exeName(tc.reported))
			}
		})
	}
}

// An empty configured binary must not match an arbitrary process. Stop falls back
// to the default name rather than passing "" through, but the comparison itself
// must not be permissive either.
func TestExeNameEmptyConfiguredDoesNotMatch(t *testing.T) {
	if exeName("") == exeName(`C:\Windows\System32\notepad.exe`) {
		t.Error("an empty configured binary matched an unrelated process")
	}
}
