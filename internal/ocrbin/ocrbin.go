// Package ocrbin carries the OpenCodeReview (ocr) binary inside release builds
// of pruefbyte, so a single download is all a developer needs.
//
// Release builds use the embedocr build tag and embed the gzip-compressed ocr
// for their platform, fetched by ./internal/ocrbin/fetch. Other builds (go
// build, go install, the Docker image, which installs ocr itself) embed nothing
// and use an ocr from the PATH.
package ocrbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed VERSION
var versionFile string

// Set by the platform's embed_*.go file in embedocr builds.
var (
	compressed []byte // gzip of the ocr binary
	sum        string // hex sha256 of the uncompressed binary, from OCR's release checksums
)

// Version is the OCR release pruefbyte is built and tested against, as pinned in VERSION (e.g. vX.Y.Z).
func Version() string { return strings.TrimSpace(versionFile) }

// Available reports whether this build carries an ocr binary.
func Available() bool { return len(compressed) > 0 }

// Path returns the embedded ocr, unpacked into the user cache directory on first
// use and verified against its release checksum.
func Path() (string, error) {
	if !Available() {
		return "", errors.New("this pruefbyte build does not include ocr")
	}
	// No fallback to the shared temp directory: another user could own the
	// directory there and swap the binary between its check and its use.
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache directory to unpack ocr into: %w", err)
	}
	return extract(filepath.Join(dir, "pruefbyte"), compressed, sum)
}

func extract(cacheDir string, gz []byte, want string) (string, error) {
	want = strings.ToLower(strings.TrimSpace(want)) // the embedded file ends with a newline
	if len(want) < 12 {
		return "", errors.New("embedded ocr has no checksum")
	}
	name := "ocr"
	if runtime.GOOS == "windows" {
		name = "ocr.exe"
	}
	dir := filepath.Join(cacheDir, "ocr-"+Version()+"-"+want[:12])
	target := filepath.Join(dir, name)
	if got, err := fileSHA256(target); err == nil && got == want {
		return target, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	// Unpack next to the target and rename, so concurrent runs never see a
	// half-written binary.
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("embedded ocr: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), zr) //nolint:gosec // G110: the archive is our own build input, checked against its checksum below
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("unpacking ocr into %s: %w", dir, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("embedded ocr has checksum %s, want %s", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // G302: an executable must be executable
		return "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		// Another run may have put it there first (Windows cannot replace a file in use).
		if got, serr := fileSHA256(target); serr == nil && got == want {
			return target, nil
		}
		return "", err
	}
	return target, nil
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
