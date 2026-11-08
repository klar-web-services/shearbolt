package grype

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klar-web-services/shearbolt/internal/testutil"
)

const goodStatus = `{"schemaVersion":"v6.1.10","from":"manual import","built":"2026-11-08T06:19:57Z","path":"/c/grype-db/6/vulnerability.db","valid":true}`

// markerRunner is a Runner on the fake Grype whose "db status" reports goodStatus. Every
// invocation is appended to the returned calls file.
func markerRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	return &Runner{
		Binary:     testutil.FakeGrype(t),
		CacheDir:   filepath.Join(dir, "cache"),
		NoDBUpdate: true,
		Environ:    append(os.Environ(), "FAKE_GRYPE_CALLS="+calls, "FAKE_GRYPE_STATUS="+goodStatus),
	}, calls
}

// with returns a copy of r whose fake Grype sees the extra variables (later values win).
func with(r *Runner, vars ...string) *Runner {
	c := *r
	c.Environ = append(append([]string{}, r.Environ...), vars...)
	return &c
}

func archiveWith(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.tar.zst")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// imports counts the "db import" invocations so far.
func imports(t *testing.T, calls string) int {
	t.Helper()
	data, err := os.ReadFile(calls)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "db import ")
}

func markerPath(r *Runner) string { return filepath.Join(r.CacheDir, ImportMarker) }

func readMarkerFile(t *testing.T, r *Runner) map[string]any {
	t.Helper()
	data, err := os.ReadFile(markerPath(r))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("marker is not JSON: %v", err)
	}
	return m
}

func wantNoMarker(t *testing.T, r *Runner) {
	t.Helper()
	if _, err := os.Lstat(markerPath(r)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker exists (%v)", err)
	}
}

// importOnce calls ImportDBIfNeeded and checks whether it imported.
func importOnce(t *testing.T, r *Runner, archive string, wantImported bool) {
	t.Helper()
	imported, err := r.ImportDBIfNeeded(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if imported != wantImported {
		t.Fatalf("imported = %v, want %v", imported, wantImported)
	}
}

func TestImportMarkerFirstScanImports(t *testing.T) {
	r, calls := markerRunner(t)
	archive := archiveWith(t, "snapshot A")
	importOnce(t, r, archive, true)
	if n := imports(t, calls); n != 1 {
		t.Fatalf("%d imports, want 1", n)
	}
	m := readMarkerFile(t, r)
	want := map[string]any{
		"archiveSha256": fmt.Sprintf("%x", sha256.Sum256([]byte("snapshot A"))),
		"grypeVersion":  Version,
		"built":         "2026-11-08T06:19:57Z",
		"schemaVersion": "v6.1.10",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("marker %s = %v, want %v", k, m[k], v)
		}
	}
}

func TestImportMarkerSkipsTheSameArchive(t *testing.T) {
	r, calls := markerRunner(t)
	archive := archiveWith(t, "snapshot A")
	importOnce(t, r, archive, true)
	importOnce(t, r, archive, false)
	importOnce(t, r, archiveWith(t, "snapshot A"), false) // same bytes at another path
	if n := imports(t, calls); n != 1 {
		t.Fatalf("%d imports, want 1", n)
	}
}

func TestImportMarkerDifferentArchiveImports(t *testing.T) {
	r, calls := markerRunner(t)
	importOnce(t, r, archiveWith(t, "snapshot A"), true)
	importOnce(t, r, archiveWith(t, "snapshot B"), true)
	if n := imports(t, calls); n != 2 {
		t.Fatalf("%d imports, want 2", n)
	}
	if got, want := readMarkerFile(t, r)["archiveSha256"], fmt.Sprintf("%x", sha256.Sum256([]byte("snapshot B"))); got != want {
		t.Fatalf("marker archiveSha256 = %v, want %v", got, want)
	}
}

func TestImportMarkerDifferentGrypeVersionImports(t *testing.T) {
	r, calls := markerRunner(t)
	archive := archiveWith(t, "snapshot A")
	importOnce(t, r, archive, true)
	m := readMarkerFile(t, r)
	m["grypeVersion"] = "0.120.0"
	data, _ := json.Marshal(m)
	if err := os.WriteFile(markerPath(r), data, 0o644); err != nil {
		t.Fatal(err)
	}
	importOnce(t, r, archive, true)
	if n := imports(t, calls); n != 2 {
		t.Fatalf("%d imports, want 2", n)
	}
	if got := readMarkerFile(t, r)["grypeVersion"]; got != Version {
		t.Fatalf("marker grypeVersion = %v, want %v", got, Version)
	}
}

// Any doubt about the cached database means importing again.
func TestImportMarkerDoubtfulDatabaseImports(t *testing.T) {
	for name, vars := range map[string][]string{
		"status fails":          {"FAKE_GRYPE_STATUS_EXIT=1"},
		"status says invalid":   {`FAKE_GRYPE_STATUS={"schemaVersion":"v6.1.10","from":"manual import","built":"2026-11-08T06:19:57Z","valid":false,"error":"checksum mismatch"}`},
		"other build time":      {`FAKE_GRYPE_STATUS={"schemaVersion":"v6.1.10","from":"manual import","built":"2026-11-09T06:19:57Z","valid":true}`},
		"other schema":          {`FAKE_GRYPE_STATUS={"schemaVersion":"v6.1.11","from":"manual import","built":"2026-11-08T06:19:57Z","valid":true}`},
		"no build time":         {`FAKE_GRYPE_STATUS={"schemaVersion":"v6.1.10","valid":true}`},
		"status is not JSON":    {"FAKE_GRYPE_STATUS=Status: valid"},
		"status prints nothing": {"FAKE_GRYPE_STATUS="},
	} {
		t.Run(name, func(t *testing.T) {
			r, calls := markerRunner(t)
			archive := archiveWith(t, "snapshot A")
			importOnce(t, r, archive, true)
			importOnce(t, with(r, vars...), archive, true)
			if n := imports(t, calls); n != 2 {
				t.Fatalf("%d imports, want 2", n)
			}
		})
	}
}

func TestImportMarkerMissingOrCorruptImports(t *testing.T) {
	for name, content := range map[string]*string{
		"missing":        nil,
		"not JSON":       ptr("{ not json"),
		"empty object":   ptr("{}"),
		"empty file":     ptr(""),
		"bad build time": ptr(`{"archiveSha256":"x","grypeVersion":"0.121.2","built":"yesterday","schemaVersion":"v6.1.10"}`),
	} {
		t.Run(name, func(t *testing.T) {
			r, calls := markerRunner(t)
			archive := archiveWith(t, "snapshot A")
			importOnce(t, r, archive, true)
			if content == nil {
				if err := os.Remove(markerPath(r)); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(markerPath(r), []byte(*content), 0o644); err != nil {
				t.Fatal(err)
			}
			importOnce(t, r, archive, true)
			if n := imports(t, calls); n != 2 {
				t.Fatalf("%d imports, want 2", n)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// The marker is written only after a successful import, and an import attempt removes the old
// one first, so a failed import never leaves a marker behind.
func TestImportMarkerFailedImportLeavesNoMarker(t *testing.T) {
	t.Run("fresh cache", func(t *testing.T) {
		r, _ := markerRunner(t)
		if _, err := with(r, "FAKE_GRYPE_IMPORT_EXIT=1").ImportDBIfNeeded(context.Background(), archiveWith(t, "snapshot A")); err == nil {
			t.Fatal("a failed import returned no error")
		}
		wantNoMarker(t, r)
	})
	t.Run("after an earlier import", func(t *testing.T) {
		r, calls := markerRunner(t)
		archive := archiveWith(t, "snapshot A")
		importOnce(t, r, archive, true)
		if _, err := with(r, "FAKE_GRYPE_IMPORT_EXIT=1").ImportDBIfNeeded(context.Background(), archiveWith(t, "snapshot B")); err == nil {
			t.Fatal("a failed import returned no error")
		}
		wantNoMarker(t, r)
		importOnce(t, r, archive, true) // A is no longer known to be in the cache
		if n := imports(t, calls); n != 3 {
			t.Fatalf("%d imports, want 3", n)
		}
	})
}

// After an import, the marker records what the database reports; if the status check fails
// then, there is no marker and the next scan imports again.
func TestImportMarkerNeedsAGoodStatusAfterImport(t *testing.T) {
	r, calls := markerRunner(t)
	archive := archiveWith(t, "snapshot A")
	importOnce(t, with(r, "FAKE_GRYPE_STATUS_EXIT=1"), archive, true)
	wantNoMarker(t, r)
	importOnce(t, r, archive, true)
	importOnce(t, r, archive, false)
	if n := imports(t, calls); n != 2 {
		t.Fatalf("%d imports, want 2", n)
	}
}

func TestImportMarkerUnreadableArchiveIsAnError(t *testing.T) {
	r, calls := markerRunner(t)
	if _, err := r.ImportDBIfNeeded(context.Background(), filepath.Join(t.TempDir(), "missing.tar.zst")); err == nil {
		t.Fatal("a missing archive returned no error")
	}
	if n := imports(t, calls); n != 0 {
		t.Fatalf("%d imports of a missing archive", n)
	}
}

func TestDBStatus(t *testing.T) {
	r, _ := markerRunner(t)
	st, err := r.DBStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Valid || st.SchemaVersion != "v6.1.10" || st.From != "manual import" || !st.Built.Equal(time.Date(2026, 11, 8, 6, 19, 57, 0, time.UTC)) {
		t.Fatalf("status = %+v", st)
	}
	if _, err := with(r, "FAKE_GRYPE_STATUS_EXIT=1").DBStatus(context.Background()); err == nil {
		t.Fatal("a failing status returned no error")
	}
}

// An archive that changes while it is imported is not recorded: the cache may hold either version.
func TestImportMarkerNotWrittenWhenTheArchiveChanged(t *testing.T) {
	r, calls := markerRunner(t)
	archive := archiveWith(t, "snapshot A")
	importOnce(t, with(r, "FAKE_GRYPE_IMPORT_APPEND= changed"), archive, true)
	wantNoMarker(t, r)
	importOnce(t, r, archive, true)
	if n := imports(t, calls); n != 2 {
		t.Fatalf("%d imports, want 2", n)
	}
}

func TestImportMarkerNeedsARegularFile(t *testing.T) {
	r, calls := markerRunner(t)
	if _, err := r.ImportDBIfNeeded(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a not-a-regular-file error for a directory", err)
	}
	link := filepath.Join(t.TempDir(), "link.tar.zst")
	if err := os.Symlink(archiveWith(t, "snapshot A"), link); err != nil {
		t.Fatal(err)
	}
	importOnce(t, r, link, true) // a symlink to a regular file is fine
	if n := imports(t, calls); n != 1 {
		t.Fatalf("%d imports, want 1", n)
	}
}
