package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateCommand(t *testing.T) {
	d := setupDeps(false, "")
	d.version = "1.2.3"
	var from string
	d.update = func(version string, stdout, stderr io.Writer) error {
		from = version
		fmt.Fprintln(stdout, "Installed shearbolt 1.3.0")
		return nil
	}
	var out, errb bytes.Buffer
	if code := run([]string{"update"}, &out, &errb, d); code != 0 || from != "1.2.3" || out.String() != "Installed shearbolt 1.3.0\n" || errb.Len() != 0 {
		t.Fatalf("exit = %d, from = %q\nstdout: %q\nstderr: %q", code, from, out.String(), errb.String())
	}

	d.update = func(string, io.Writer, io.Writer) error { return errors.New("nothing was updated") }
	out.Reset()
	if code := run([]string{"update"}, &out, &errb, d); code != 2 || errb.String() != "shearbolt update: nothing was updated\n" {
		t.Fatalf("exit = %d\nstderr: %q", code, errb.String())
	}
}

func TestUpdateArguments(t *testing.T) {
	d := setupDeps(false, "")
	d.update = func(string, io.Writer, io.Writer) error { t.Fatal("update must not run"); return nil }
	var out, errb bytes.Buffer
	if code := run([]string{"update", "-h"}, &out, &errb, d); code != 0 || !strings.HasPrefix(out.String(), "Usage: shearbolt update\n") || errb.Len() != 0 {
		t.Errorf("-h: exit = %d\nstdout: %q\nstderr: %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"update", "1.2.3"}, &out, &errb, d); code != 2 || out.Len() != 0 || !strings.HasPrefix(errb.String(), `shearbolt update: unexpected argument "1.2.3"`) {
		t.Errorf("argument: exit = %d\nstdout: %q\nstderr: %q", code, out.String(), errb.String())
	}
	out.Reset()
	run([]string{"help"}, &out, &errb, d)
	if !strings.Contains(out.String(), "\n  shearbolt update                     Update shearbolt to the latest release\n") {
		t.Errorf("usage:\n%s", out.String())
	}
}

func updateRelease(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl is not installed: %v", err)
	}
	arch, ok := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	os_, okOS := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin"}[runtime.GOOS]
	if !ok || !okOS {
		t.Skipf("no release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	script := "#!/bin/sh\necho 'shearbolt " + version + "'\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "shearbolt", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	name := "shearbolt-" + arch + "-" + os_ + ".tar.gz"
	site := t.TempDir()
	if err := os.Mkdir(filepath.Join(site, "v"+version), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"latest":                         []byte(version + "\n"),
		"v" + version + "/" + name:       buf.Bytes(),
		"v" + version + "/checksums.txt": fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(buf.Bytes()), name),
	} {
		if err := os.WriteFile(filepath.Join(site, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(site)))
	t.Cleanup(srv.Close)
	return srv.URL
}

func installedBinary(t *testing.T, name string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestUpdateBinary(t *testing.T) {
	t.Setenv("SHEARBOLT_BASE_URL", updateRelease(t, "1.3.0"))
	t.Setenv("SHEARBOLT_INSTALL_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("SHEARBOLT_VERSION", "")

	t.Run("older", func(t *testing.T) {
		exe := installedBinary(t, "shearbolt")
		var out, errb bytes.Buffer
		if err := updateBinary(exe, "1.2.3", &out, &errb); err != nil || out.String() != "Installed shearbolt 1.3.0 to "+exe+"\n" || errb.Len() != 0 {
			t.Fatalf("err = %v\nstdout: %q\nstderr: %q", err, out.String(), errb.String())
		}
		if got, err := exec.Command(exe).Output(); err != nil || string(got) != "shearbolt 1.3.0\n" {
			t.Fatalf("the binary prints %q (%v)", got, err)
		}
	})
	t.Run("up to date", func(t *testing.T) {
		exe := installedBinary(t, "shearbolt")
		var out, errb bytes.Buffer
		if err := updateBinary(exe, "1.3.0", &out, &errb); err != nil || out.String() != "shearbolt 1.3.0 is already up to date.\n" || errb.Len() != 0 {
			t.Fatalf("err = %v\nstdout: %q\nstderr: %q", err, out.String(), errb.String())
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Fatalf("the binary changed: %q", got)
		}
	})
	t.Run("renamed binary", func(t *testing.T) {
		exe := installedBinary(t, "sb")
		var out, errb bytes.Buffer
		err := updateBinary(exe, "1.2.3", &out, &errb)
		if err == nil || !strings.Contains(err.Error(), "is not named shearbolt") || out.Len() != 0 {
			t.Fatalf("err = %v\nstdout: %q", err, out.String())
		}
		if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
			t.Fatalf("the folder holds %d entries", len(entries))
		}
	})
	t.Run("installer fails", func(t *testing.T) {
		t.Setenv("SHEARBOLT_BASE_URL", os.Getenv("SHEARBOLT_BASE_URL")+"/missing")
		exe := installedBinary(t, "shearbolt")
		var out, errb bytes.Buffer
		err := updateBinary(exe, "1.2.3", &out, &errb)
		if err == nil || err.Error() != "nothing was updated" || !strings.Contains(errb.String(), "shearbolt installer: could not determine the latest version") {
			t.Fatalf("err = %v\nstderr: %q", err, errb.String())
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Fatalf("the binary changed: %q", got)
		}
	})
}
