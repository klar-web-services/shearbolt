package grype

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const ImportMarker = "grype-db-import.json"

type importMarker struct {
	ArchiveSHA256 string `json:"archiveSha256"`
	GrypeVersion  string `json:"grypeVersion"`
	Built         string `json:"built"`
	SchemaVersion string `json:"schemaVersion"`
}

type DBStatus struct {
	SchemaVersion string
	From          string
	Built         time.Time
	Valid         bool
}

// A database that does not exist or cannot be read makes Grype exit non-zero, which is an error
// here.
func (r *Runner) DBStatus(ctx context.Context) (DBStatus, error) {
	var out bytes.Buffer
	if err := r.runOutput(ctx, []string{"db", "status", "-o", "json"}, &out); err != nil {
		return DBStatus{}, err
	}
	var raw struct {
		SchemaVersion string `json:"schemaVersion"`
		From          string `json:"from"`
		Built         string `json:"built"`
		Valid         bool   `json:"valid"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return DBStatus{}, fmt.Errorf("unexpected output from grype db status: %w", err)
	}
	built, err := time.Parse(time.RFC3339, raw.Built)
	if err != nil {
		return DBStatus{}, fmt.Errorf("grype db status reports no valid build time: %q", raw.Built)
	}
	return DBStatus{SchemaVersion: raw.SchemaVersion, From: raw.From, Built: built.UTC(), Valid: raw.Valid}, nil
}

// Any doubt means importing again. The old marker is removed before an import and a new one
// written only after it succeeded and the archive still has the same SHA-256, so a failed
// import leaves no marker.
func (r *Runner) ImportDBIfNeeded(ctx context.Context, archive string) (bool, error) {
	archive, err := filepath.Abs(archive)
	if err != nil {
		return false, err
	}
	sum, err := regularFileSHA256(archive)
	if err != nil {
		return false, err
	}
	cacheDir, err := filepath.Abs(r.CacheDir)
	if err != nil {
		return false, err
	}
	marker := filepath.Join(cacheDir, ImportMarker)
	if r.alreadyImported(ctx, marker, sum) {
		return false, nil
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove the old import marker: %w", err)
	}
	if err := r.ImportDB(ctx, archive); err != nil {
		return true, err
	}
	st, err := r.DBStatus(ctx)
	if err != nil || !st.Valid {
		return true, nil // imported, but not recorded: the next scan imports again
	}
	if after, err := regularFileSHA256(archive); err != nil || after != sum {
		return true, nil // the archive changed during the import, so it is unknown which one the cache holds
	}
	m := importMarker{ArchiveSHA256: sum, GrypeVersion: Version, Built: st.Built.Format(time.RFC3339), SchemaVersion: st.SchemaVersion}
	_ = writeMarker(marker, m) // without a marker, the next scan just imports again
	return true, nil
}

func (r *Runner) alreadyImported(ctx context.Context, marker, sum string) bool {
	data, err := os.ReadFile(marker)
	if err != nil {
		return false
	}
	var m importMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	built, err := time.Parse(time.RFC3339, m.Built)
	if err != nil || m.ArchiveSHA256 != sum || m.GrypeVersion != Version || m.SchemaVersion == "" {
		return false
	}
	st, err := r.DBStatus(ctx)
	return err == nil && st.Valid && st.Built.Equal(built) && st.SchemaVersion == m.SchemaVersion
}

// The file is opened without blocking and checked before it is read, so a FIFO or a device
// cannot make the scan wait forever.
func regularFileSHA256(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("read the database archive: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read the database archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the database archive %s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read the database archive: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeMarker(path string, m importMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ImportMarker+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
