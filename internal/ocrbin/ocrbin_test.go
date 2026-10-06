package ocrbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"strings"
	"testing"
)

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(data)
	zw.Close()
	return b.Bytes()
}

func TestExtract(t *testing.T) {
	bin := []byte("#!/bin/sh\necho fake ocr\n")
	h := sha256.Sum256(bin)
	sum := hex.EncodeToString(h[:])
	cache := t.TempDir()

	p, err := extract(cache, gzipped(t, bin), sum+"\n") // as embedded from ocr.sha256
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, bin) {
		t.Errorf("unpacked %q", got)
	}
	if !strings.Contains(p, "ocr-"+Version()+"-"+sum[:12]) {
		t.Errorf("path %s does not name version and checksum", p)
	}
	if fi, err := os.Stat(p); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("not executable: %v %v", fi.Mode(), err)
	}

	// Reused while intact; replaced when the cached copy was changed.
	if p2, err := extract(cache, gzipped(t, bin), sum); err != nil || p2 != p {
		t.Errorf("second extract: %s, %v", p2, err)
	}
	os.WriteFile(p, []byte("tampered"), 0o755)
	if _, err := extract(cache, gzipped(t, bin), sum); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, bin) {
		t.Error("tampered cache copy was not replaced")
	}

	// A payload that does not match its checksum is refused.
	if _, err := extract(t.TempDir(), gzipped(t, []byte("other")), sum); err == nil {
		t.Error("checksum mismatch accepted")
	}
}

func TestVersionPinned(t *testing.T) {
	if v := Version(); !strings.HasPrefix(v, "v") || strings.ContainsAny(v, " \n") {
		t.Errorf("VERSION = %q", v)
	}
}
