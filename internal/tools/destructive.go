package tools

import (
	"encoding/json"
	"regexp"
	"strings"
)

// IsDestructive reports whether a tool call performs an irreversible change that
// cannot be reconstructed from the code in the working tree - the only class of
// action that still needs explicit human approval under auto mode. Only bash is
// classified: edit_file/write_file change tracked code (recoverable from git and
// the session's recorded file changes), so they are considered reversible.
func IsDestructive(name string, rawArgs json.RawMessage) bool {
	if name != "bash" {
		return false
	}
	var a struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal(rawArgs, &a) != nil {
		// Unparseable args: treat as destructive so a malformed call cannot slip
		// an irreversible command past auto mode.
		return true
	}
	return DestructiveBash(a.Cmd)
}

// destructiveCmds are commands that destroy data when they are the command of a
// pipeline segment (matched as the leading token, so "format"/"npm" never trip
// the "rm" rule).
var destructiveCmds = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true,
	"truncate": true, "dd": true, "mkfs": true, "srm": true,
}

// destructivePatterns are multi-word / flagged forms that irreversibly drop data
// or rewrite history. Matched as substrings of the whole (lowercased) command.
var destructivePatterns = []string{
	"git reset --hard", "git clean", "git checkout --", "git checkout .",
	"git restore", "git stash drop", "git stash clear", "git push --force",
	"git push -f", "git branch -d", // -D lowercases to -d
	"-delete", "-exec rm", "drop table", "drop database", "truncate table",
	"delete from", "docker volume rm", "docker system prune", "kubectl delete",
}

// segmentSplit breaks a command line into pipeline/sequence segments so the
// leading command of each can be checked independently. It also splits on
// command-substitution boundaries - parentheses and backticks - so a dangerous
// command hidden in "$(...)" or backticks (e.g. echo $(dd ...)) still surfaces
// as the leading token of its own segment instead of slipping through.
var segmentSplit = regexp.MustCompile("[|&;()`\n]+")

// leadingToken strips common prefixes (sudo, env assignments) and returns the
// first bare command word of a segment.
func leadingToken(seg string) string {
	for {
		seg = strings.TrimSpace(seg)
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			return ""
		}
		head := fields[0]
		if head == "sudo" || head == "command" || head == "nice" || head == "time" {
			seg = strings.TrimSpace(strings.TrimPrefix(seg, head))
			continue
		}
		// VAR=val prefix before the real command.
		if strings.ContainsRune(head, '=') && !strings.ContainsAny(head, "/") {
			seg = strings.TrimSpace(strings.TrimPrefix(seg, head))
			continue
		}
		return head
	}
}

// DestructiveBash reports whether a shell command irreversibly destroys data
// that cannot be restored from the codebase (file/dir deletion, history rewrite,
// dropping/truncating tables, etc.). It errs toward true: a false positive only
// costs an extra confirmation, a false negative loses data silently.
func DestructiveBash(cmd string) bool {
	lc := strings.ToLower(cmd)
	for _, p := range destructivePatterns {
		if strings.Contains(lc, p) {
			return true
		}
	}
	for _, seg := range segmentSplit.Split(cmd, -1) {
		head := leadingToken(seg)
		if head == "" {
			continue
		}
		// Strip a leading path so "/bin/rm" matches "rm".
		if i := strings.LastIndexByte(head, '/'); i >= 0 {
			head = head[i+1:]
		}
		if destructiveCmds[strings.ToLower(head)] {
			return true
		}
	}
	return false
}
