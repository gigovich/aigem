package tools

import (
	"encoding/json"
	"testing"
)

func TestDestructiveBash(t *testing.T) {
	destructive := []string{
		"rm file.txt",
		"rm -rf build",
		"sudo rm -rf /var/data",
		"/bin/rm x",
		"rmdir empty",
		"find . -name '*.tmp' -delete",
		"find . -type f -exec rm {} +",
		"git reset --hard HEAD~1",
		"git clean -fd",
		"git checkout -- internal/foo.go",
		"git checkout .",
		"git restore .",
		"git push --force origin main",
		"git push -f",
		"truncate -s 0 log.txt",
		"dd if=/dev/zero of=disk.img",
		"psql -c 'DROP TABLE users'",
		"echo 'TRUNCATE TABLE events' | psql",
		"docker volume rm pgdata",
		"FOO=bar rm baz",
	}
	for _, c := range destructive {
		if !DestructiveBash(c) {
			t.Errorf("DestructiveBash(%q) = false, want true", c)
		}
	}

	safe := []string{
		"ls -la",
		"go test ./...",
		"npm run build",
		"gofmt -w .",
		"git status",
		"git commit -m 'wip'",
		"git checkout -b feature", // new branch, not a discard
		"grep -rn rm .",           // 'rm' only as an argument, not the command
		"cat README.md",
		"go build -o bin/aigem ./cmd/...",
		"echo formatting",
	}
	for _, c := range safe {
		if DestructiveBash(c) {
			t.Errorf("DestructiveBash(%q) = true, want false", c)
		}
	}
}

func TestIsDestructiveOnlyBash(t *testing.T) {
	if IsDestructive("edit_file", json.RawMessage(`{"path":"a.go","old_string":"x","new_string":"y"}`)) {
		t.Error("edit_file must be treated as reversible")
	}
	if IsDestructive("write_file", json.RawMessage(`{"path":"a.go","content":""}`)) {
		t.Error("write_file must be treated as reversible")
	}
	if !IsDestructive("bash", json.RawMessage(`{"cmd":"rm -rf node_modules"}`)) {
		t.Error("destructive bash must be flagged")
	}
	if IsDestructive("bash", json.RawMessage(`{"cmd":"go test ./..."}`)) {
		t.Error("safe bash must not be flagged")
	}
	// Malformed bash args err toward destructive.
	if !IsDestructive("bash", json.RawMessage(`{"cmd":`)) {
		t.Error("unparseable bash args should be treated as destructive")
	}
}

func TestDeniedBashPattern(t *testing.T) {
	// Hard-blocked: dangerous binary as the command head, or rm -rf of / or cwd.
	denied := []string{
		"dd if=/dev/zero of=disk.img",
		"mkfs.ext4 /dev/sda1",
		"shred -u secret",
		"sudo fdisk /dev/sda",
		"rm -rf /",
		"echo hi && rm -rf .",
		"cat x | dd if=/dev/zero of=/dev/sda",
		"echo $(dd if=/dev/zero of=/dev/sda)",
		"echo `mkfs.ext4 /dev/sda1`",
		"/sbin/mkfs.ext4 /dev/sda1",
	}
	for _, c := range denied {
		if deniedBashPattern(c) == "" {
			t.Errorf("deniedBashPattern(%q) = \"\", want a match", c)
		}
	}

	// Allowed: the dangerous token only appears inside an argument, not as a
	// command. The API-key case is the regression that motivated the fix - "dd"
	// is a substring of the token but never a command head.
	allowed := []string{
		// fake token; the "dd" substring is the regression trigger, not the value.
		"curl -s -H 'X-API-Key: example_api_0000000000000000000000000000dd00' https://example.invalid/api/v1/users/me/",
		"echo add middleware",
		"go test ./...",
		"git add .",
		"ddrescue src dst", // ddrescue is not dd
	}
	for _, c := range allowed {
		if p := deniedBashPattern(c); p != "" {
			t.Errorf("deniedBashPattern(%q) = %q, want \"\"", c, p)
		}
	}
}
