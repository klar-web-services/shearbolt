package grype

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func server(t *testing.T, archive []byte, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if !strings.HasSuffix(r.URL.Path, "/v9.9.9/grype_9.9.9_linux_amd64.tar.gz") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const fakeBinary = "#!/bin/sh\necho fake\n"

func testInstaller(t *testing.T, baseURL, checksum string) *Installer {
	in := NewInstaller(t.TempDir())
	in.BaseURL, in.Version, in.OS, in.Arch = baseURL, "9.9.9", "linux", "amd64"
	in.Checksums = map[string]string{"linux_amd64": checksum}
	in.BinaryChecksums = map[string]string{"linux_amd64": sum([]byte(fakeBinary))}
	return in
}

func noGrypeFile(t *testing.T, cacheDir string) {
	t.Helper()
	_ = filepath.WalkDir(cacheDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("unexpected file left behind: %s", p)
		}
		return nil
	})
}

func TestEnsureDownloadsVerifiesAndCaches(t *testing.T) {
	archive := tarGz(t, map[string]string{"README.md": "hi", "grype": fakeBinary})
	var hits int32
	in := testInstaller(t, server(t, archive, &hits).URL, sum(archive))
	bin, err := in.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bin)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("binary missing or not executable: %v %v", info, err)
	}
	if _, err := in.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits := atomic.LoadInt32(&hits); hits != 1 {
		t.Fatalf("downloads = %d, want 1 (second call must use the cache)", hits)
	}
}

func TestEnsureRejectsChecksumMismatch(t *testing.T) {
	archive := tarGz(t, map[string]string{"grype": "evil"})
	var hits int32
	in := testInstaller(t, server(t, archive, &hits).URL, strings.Repeat("0", 64))
	if _, err := in.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want checksum error", err)
	}
	noGrypeFile(t, in.CacheDir)
}

func TestEnsureRejectsWrongBinaryInVerifiedArchive(t *testing.T) {
	archive := tarGz(t, map[string]string{"grype": "evil"})
	var hits int32
	in := testInstaller(t, server(t, archive, &hits).URL, sum(archive))
	if _, err := in.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want checksum error", err)
	}
	noGrypeFile(t, in.CacheDir)
}

func TestEnsureRedownloadsTamperedBinary(t *testing.T) {
	archive := tarGz(t, map[string]string{"grype": fakeBinary})
	var hits int32
	in := testInstaller(t, server(t, archive, &hits).URL, sum(archive))
	bin, err := in.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits := atomic.LoadInt32(&hits); hits != 2 {
		t.Fatalf("downloads = %d, want 2", hits)
	}
}

func TestEnsureUnsupportedPlatform(t *testing.T) {
	in := testInstaller(t, "http://unused", "x")
	in.OS = "plan9"
	if _, err := in.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("err = %v", err)
	}
}
