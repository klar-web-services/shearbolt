package grype

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"time"
)

// DefaultBaseURL is where official Grype release archives are published.
const DefaultBaseURL = "https://github.com/anchore/grype/releases/download"

const maxArchiveBytes = 200 << 20

// Installer downloads and verifies the pinned Grype release into a cache directory.
type Installer struct {
	CacheDir  string
	BaseURL   string
	Version   string
	Checksums map[string]string
	// BinaryChecksums holds the SHA-256 of the grype executable inside each archive, keyed like Checksums.
	BinaryChecksums map[string]string
	OS, Arch        string
	Client          *http.Client
}

// NewInstaller returns an Installer for this release's pinned Grype on the current platform.
func NewInstaller(cacheDir string) *Installer {
	return &Installer{
		CacheDir:        cacheDir,
		BaseURL:         DefaultBaseURL,
		Version:         Version,
		Checksums:       Checksums,
		BinaryChecksums: BinaryChecksums,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Client:          &http.Client{Timeout: 5 * time.Minute},
	}
}

// Ensure returns the path of a verified Grype binary, downloading it when the cache is missing or altered.
func (in *Installer) Ensure(ctx context.Context) (string, error) {
	key := in.OS + "_" + in.Arch
	want, ok := in.Checksums[key]
	wantBin, okBin := in.BinaryChecksums[key]
	if !ok || !okBin {
		return "", fmt.Errorf("Shearbolt has no pinned Grype build for %s/%s", in.OS, in.Arch)
	}
	dir := filepath.Join(in.CacheDir, "grype", in.Version, key)
	bin := filepath.Join(dir, "grype")
	if got, err := fileSHA256(bin); err == nil && got == wantBin {
		return bin, nil
	}
	name := fmt.Sprintf("grype_%s_%s.tar.gz", in.Version, key)
	archive, err := in.download(ctx, fmt.Sprintf("%s/v%s/%s", in.BaseURL, in.Version, name))
	if err != nil {
		return "", fmt.Errorf("download Grype %s: %w", in.Version, err)
	}
	if got := bytesSHA256(archive); got != want {
		return "", fmt.Errorf("Grype archive %s failed checksum verification (expected %s, got %s)", name, want, got)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := extractGrype(archive, dir)
	if err != nil {
		return "", fmt.Errorf("unpack Grype: %w", err)
	}
	defer os.Remove(tmp)
	binSum, err := fileSHA256(tmp)
	if err != nil {
		return "", err
	}
	if binSum != wantBin {
		return "", fmt.Errorf("the Grype executable in %s failed checksum verification (expected %s, got %s)", name, wantBin, binSum)
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	return bin, nil
}

func (in *Installer) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := in.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArchiveBytes {
		return nil, errors.New("the archive is unexpectedly large")
	}
	return data, nil
}

// extractGrype writes the archive's "grype" executable to a temporary file in dir and returns its path.
func extractGrype(archive []byte, dir string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("the archive does not contain a grype executable")
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != "grype" {
			continue
		}
		if hdr.Size > maxArchiveBytes {
			return "", errors.New("the grype executable in the archive is unexpectedly large")
		}
		out, err := os.CreateTemp(dir, "grype-*.tmp")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		if err := out.Chmod(0o755); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		if err := out.Close(); err != nil {
			os.Remove(out.Name())
			return "", err
		}
		return out.Name(), nil
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func bytesSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
