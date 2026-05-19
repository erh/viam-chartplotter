package vc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
)

// TestWeatherCacheCleanerRespectsAge verifies the cleaner deletes
// files past the cutoff and leaves fresh ones (including those in
// nested subdirectories like raw-ecmwf/). Locks in the 60-day rule
// from module.go so a future "let's be aggressive and clean at 7d"
// change has to update the test explicitly.
func TestWeatherCacheCleanerRespectsAge(t *testing.T) {
	dir := t.TempDir()
	wc, err := NewWeatherCache(dir, logging.NewTestLogger(t))
	if err != nil {
		t.Fatalf("NewWeatherCache: %v", err)
	}

	// Build a small fixture: one fresh JSON, one stale JSON, one
	// fresh raw GRIB, one stale raw GRIB, all in the layout the
	// real cache uses. Use distinctive content so we can verify
	// the survivors are exactly the ones we expect.
	fresh := filepath.Join(dir, "ecmwf-v3-f000.json")
	stale := filepath.Join(dir, "ecmwf-v2-f000.json")
	rawDir := filepath.Join(dir, "raw-ecmwf")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	freshRaw := filepath.Join(rawDir, "ecmwf-20260519T00-f000.grib2")
	staleRaw := filepath.Join(rawDir, "ecmwf-20251201T00-f000.grib2")
	now := time.Now()
	for _, path := range []string{fresh, stale, freshRaw, staleRaw} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the "stale" files past the cleanup cutoff. 90 days
	// is safely past the 60-day production threshold so the
	// production setting still passes this test.
	old := now.Add(-90 * 24 * time.Hour)
	for _, path := range []string{stale, staleRaw} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	deleted, freed := wc.cleanOldFiles(60 * 24 * time.Hour)
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the two backdated files)", deleted)
	}
	if freed != 2 {
		t.Errorf("freed = %d bytes, want 2 (each file was 1 byte)", freed)
	}
	// Fresh files must survive.
	for _, path := range []string{fresh, freshRaw} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to survive: %v", path, err)
		}
	}
	// Stale files must be gone.
	for _, path := range []string{stale, staleRaw} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted: stat err=%v", path, err)
		}
	}
}

// TestWeatherCacheCleanerEmptyDir asserts the cleaner handles an
// empty cache directory without errors and reports zero deletes —
// guards against a Walk-on-empty regression introducing a spurious
// "removed 0 files" log line.
func TestWeatherCacheCleanerEmptyDir(t *testing.T) {
	dir := t.TempDir()
	wc, err := NewWeatherCache(dir, logging.NewTestLogger(t))
	if err != nil {
		t.Fatalf("NewWeatherCache: %v", err)
	}
	deleted, freed := wc.cleanOldFiles(time.Hour)
	if deleted != 0 || freed != 0 {
		t.Errorf("empty dir: deleted=%d freed=%d, want 0/0", deleted, freed)
	}
}
