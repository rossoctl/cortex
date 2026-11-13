package updatestatus

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.9.5":        true,
		"dev":           false,
		"dev-abc+dirty": false,
		"main-1a2b3c4":  false,
		"v0.9.6-rc.1":   false,
		"v0.9":          false,
		"":              false,
	} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	const body = `{"checkedAt":"2026-11-12T15:00:00Z","latest":"v0.9.6","installed":"v0.9.5","result":"waiting","installAfter":"2026-11-14T10:00:00Z","error":""}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Status{
		CheckedAt:    time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC),
		Latest:       "v0.9.6",
		Installed:    "v0.9.5",
		Result:       ResultWaiting,
		InstallAfter: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
	}
	if !s.CheckedAt.Equal(want.CheckedAt) || !s.InstallAfter.Equal(want.InstallAfter) ||
		s.Latest != want.Latest || s.Installed != want.Installed || s.Result != want.Result || s.Error != "" {
		t.Errorf("Read = %+v, want %+v", s, want)
	}
}

func TestReadMissing(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), FileName))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read of a missing file: err = %v, want fs.ErrNotExist", err)
	}
}
