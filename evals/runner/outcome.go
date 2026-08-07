package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CheckOutcome verifies that the run did the work, as opposed to only handling
// delegation well. It reads the run's own workspace copy, so it must be called
// before that copy is removed. before is the workspace digest taken right after
// the fixture was copied.
//
// Failures found here set r.OutcomeFailed, which withholds credit in every
// rate: "did not delegate" is a good answer only when the task still got done.
func CheckOutcome(s Scenario, r *Run, dir string, before map[string]string) {
	var fail []string

	for _, want := range s.Expect.AnswerContains {
		if !strings.Contains(strings.ToLower(r.Answer), strings.ToLower(want)) {
			fail = append(fail, fmt.Sprintf("the answer never mentions %q", want))
		}
	}
	for _, path := range sortedKeys(s.Expect.FileContains) {
		want := s.Expect.FileContains[path]
		body, err := readWorkspaceFile(dir, path)
		if err != nil {
			fail = append(fail, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if !strings.Contains(body, want) {
			fail = append(fail, fmt.Sprintf("%s does not contain %q", path, want))
		}
	}
	for _, path := range sortedKeys(s.Expect.FileAbsent) {
		unwanted := s.Expect.FileAbsent[path]
		body, err := readWorkspaceFile(dir, path)
		if err != nil {
			fail = append(fail, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if strings.Contains(body, unwanted) {
			fail = append(fail, fmt.Sprintf("%s still contains %q", path, unwanted))
		}
	}
	if s.Expect.Unchanged || s.Expect.Changed {
		after, err := TreeDigest(dir)
		changed := diffDigests(before, after)
		switch {
		case err != nil:
			fail = append(fail, "could not re-read the workspace: "+err.Error())
		case s.Expect.Unchanged && len(changed) > 0:
			fail = append(fail, "the run was told not to edit but changed "+strings.Join(changed, ", "))
		case s.Expect.Changed && len(changed) == 0:
			fail = append(fail, "the run left the workspace untouched")
		}
	}

	if len(fail) > 0 {
		r.OutcomeFailed = true
		r.Failures = append(r.Failures, fail...)
	}
}

// readWorkspaceFile reads a scenario-declared path from the run's workspace. The
// path comes from the scenario file, not from the model, but it is still
// resolved against the workspace so a stray "../" cannot read outside it.
func readWorkspaceFile(dir, rel string) (string, error) {
	full := filepath.Join(dir, filepath.Clean("/"+rel))
	body, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TreeDigest maps each regular file's workspace-relative path to a digest of
// its contents.
func TreeDigest(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

// diffDigests names the files that were added, removed, or edited.
func diffDigests(before, after map[string]string) []string {
	var changed []string
	for path, sum := range after {
		if old, ok := before[path]; !ok {
			changed = append(changed, path+" (added)")
		} else if old != sum {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path+" (removed)")
		}
	}
	sort.Strings(changed)
	return changed
}

// sortedKeys gives map iteration a stable order, so repeated runs report their
// failures identically.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
