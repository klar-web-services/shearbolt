//go:build unix

package skill

import (
	"syscall"
	"testing"
)

func TestInstallModesIgnoreUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	dir := Dir(t.TempDir())
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	checkModes(t, dir)
}
