//go:build unix

package grype

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO is refused at once instead of blocking the scan while it waits for a writer.
func TestImportMarkerRefusesAFIFO(t *testing.T) {
	r, calls := markerRunner(t)
	fifo := filepath.Join(t.TempDir(), "db.tar.zst")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.ImportDBIfNeeded(context.Background(), fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a FIFO")
	}
	if n := imports(t, calls); n != 0 {
		t.Fatalf("%d imports of a FIFO", n)
	}
}
