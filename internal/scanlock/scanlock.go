//go:build unix

// Package scanlock makes scans that share one Shearbolt cache run one after another.
//
// A scan installs Grype, imports or updates the vulnerability database and scans with it, all in
// the cache. Two concurrent scans could otherwise replace each other's database mid-scan, or tie
// one archive's import marker to another archive's database.
package scanlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// File is the lock file inside the cache.
const File = "shearbolt.lock"

// Acquire takes an exclusive advisory lock (flock) on <dir>/shearbolt.lock, creating the folder
// and the file as needed. When another process holds the lock, Acquire writes one line to
// waiting and blocks until it is free. The returned function releases the lock; the kernel also
// releases it when the process exits, so a crashed scan never leaves the cache locked.
func Acquire(dir string, waiting io.Writer) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the cache %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, File), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the cache lock: %w", err)
	}
	fd := int(f.Fd())
	err = flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		fmt.Fprintf(waiting, "waiting for another shearbolt scan that uses %s\n", dir)
		err = flock(fd, syscall.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock the cache %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		f.Close()
	}, nil
}

func flock(fd, how int) error {
	for {
		if err := syscall.Flock(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
