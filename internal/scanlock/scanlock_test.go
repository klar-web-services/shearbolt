//go:build unix

package scanlock

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for one writer goroutine and a reader.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestAcquireCreatesTheLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "cache")
	var waiting bytes.Buffer
	release, err := Acquire(dir, &waiting)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Stat(filepath.Join(dir, File)); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	if waiting.Len() > 0 {
		t.Fatalf("a free lock printed %q", waiting.String())
	}
}

// A busy lock prints the waiting line once and blocks until the holder releases it.
func TestAcquireWaitsForTheHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(dir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var waiting syncBuffer
	acquired := make(chan func())
	go func() {
		r, err := Acquire(dir, &waiting)
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		acquired <- r
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(waiting.String(), "waiting") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-acquired:
		t.Fatal("acquired a lock that is held")
	case <-time.After(200 * time.Millisecond):
	}
	if want := "waiting for another shearbolt scan that uses " + dir + "\n"; waiting.String() != want {
		t.Fatalf("waiting output = %q, want %q", waiting.String(), want)
	}
	release()
	select {
	case r := <-acquired:
		if r != nil {
			r()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not acquired after the holder released the lock")
	}
}

func TestSequentialAcquiresDoNotWait(t *testing.T) {
	dir := t.TempDir()
	var waiting bytes.Buffer
	for range 3 {
		release, err := Acquire(dir, &waiting)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if waiting.Len() > 0 {
		t.Fatalf("sequential acquires printed %q", waiting.String())
	}
}

func TestAcquireFailsOnAnUnusableFolder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(filepath.Join(file, "cache"), &bytes.Buffer{}); err == nil {
		t.Fatal("no error for a cache below a regular file")
	}
}
