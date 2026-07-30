package llm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFileForTest(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestSaveAndLoadLimits(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().Truncate(time.Second)

	openai := Limits{Provider: "openai", Plan: "prolite", ObservedAt: now,
		Windows: []LimitWindow{{Name: "primary", UsedPercent: 2, WindowMinutes: 10080}}}
	if err := SaveLimits(openai); err != nil {
		t.Fatal(err)
	}
	// A second provider must not displace the first.
	if err := SaveLimits(Limits{Provider: "xai", ObservedAt: now,
		Windows: []LimitWindow{{Name: "primary", UsedPercent: 40}}}); err != nil {
		t.Fatal(err)
	}

	all := LoadLimits()
	if len(all) != 2 {
		t.Fatalf("stored providers = %v", all)
	}
	if got := all["openai"]; got.Plan != "prolite" || len(got.Windows) != 1 || got.Windows[0].UsedPercent != 2 {
		t.Fatalf("openai snapshot = %+v", got)
	}
}

func TestSaveLimitsKeepsTheNewestObservation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	newer := time.Now().Truncate(time.Second)
	older := newer.Add(-time.Hour)

	if err := SaveLimits(Limits{Provider: "openai", ObservedAt: newer,
		Windows: []LimitWindow{{Name: "primary", UsedPercent: 9}}}); err != nil {
		t.Fatal(err)
	}
	// Five bots write this file; one of them holding a stale reading must not
	// roll the report backwards.
	if err := SaveLimits(Limits{Provider: "openai", ObservedAt: older,
		Windows: []LimitWindow{{Name: "primary", UsedPercent: 1}}}); err != nil {
		t.Fatal(err)
	}
	if got := LoadLimits()["openai"]; got.Windows[0].UsedPercent != 9 {
		t.Fatalf("an older observation overwrote a newer one: %+v", got)
	}
}

func TestSaveLimitsIgnoresEmptyReading(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	good := Limits{Provider: "openai", ObservedAt: time.Now(),
		Windows: []LimitWindow{{Name: "primary", UsedPercent: 3}}}
	if err := SaveLimits(good); err != nil {
		t.Fatal(err)
	}
	// A provider that reports no quota headers (or a failed call) must not erase
	// what an earlier call learned.
	if err := SaveLimits(Limits{Provider: "openai", ObservedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := LoadLimits()["openai"]; len(got.Windows) != 1 {
		t.Fatalf("empty reading erased the snapshot: %+v", got)
	}
}

func TestLoadLimitsToleratesGarbage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dirPath, err := limitsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileForTest(filepath.Join(dirPath, "openai.json"), "not json"); err != nil {
		t.Fatal(err)
	}
	if got := LoadLimits(); len(got) != 0 {
		t.Fatalf("expected an empty map, got %v", got)
	}
}
