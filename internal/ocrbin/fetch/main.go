// Command fetch downloads the OCR release pinned in internal/ocrbin/VERSION for
// every platform pruefbyte ships, checks each binary against the release's
// sha256sum.txt and stores it gzip-compressed for embedding:
//
//	internal/ocrbin/bin/<os>_<arch>/ocr.gz      the binary, gzipped
//	internal/ocrbin/bin/<os>_<arch>/ocr.sha256  its checksum
//	internal/ocrbin/bin/LICENSE                 OCR's license, shipped with releases
//
// GoReleaser runs it before release builds: go run ./internal/ocrbin/fetch
// Binaries already present with the right checksum are not downloaded again.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const repo = "https://github.com/alibaba/open-code-review"

// Platforms pruefbyte releases for; keep in sync with .goreleaser.yaml and embed_*.go.
var platforms = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64", "windows_arm64"}

func main() {
	if err := run("internal/ocrbin"); err != nil {
		fmt.Fprintln(os.Stderr, "fetch ocr:", err)
		os.Exit(1)
	}
}

var client = &http.Client{Timeout: 5 * time.Minute}

func run(pkgDir string) error {
	v, err := os.ReadFile(filepath.Join(pkgDir, "VERSION"))
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(v))
	sums, err := download(fmt.Sprintf("%s/releases/download/%s/sha256sum.txt", repo, version))
	if err != nil {
		return err
	}
	want := parseSums(sums)
	binDir := filepath.Join(pkgDir, "bin")
	fetched := false
	for _, p := range platforms {
		asset := "opencodereview-" + strings.ReplaceAll(p, "_", "-")
		if strings.HasPrefix(p, "windows") {
			asset += ".exe"
		}
		sum := want[asset]
		if sum == "" {
			return fmt.Errorf("%s has no checksum in %s's sha256sum.txt", asset, version)
		}
		dir := filepath.Join(binDir, p)
		// Check the payload itself, not just the recorded checksum, so a truncated
		// ocr.gz (an interrupted run, a stale CI cache) is downloaded again.
		if cur, err := os.ReadFile(filepath.Join(dir, "ocr.sha256")); err == nil && strings.TrimSpace(string(cur)) == sum {
			if got, err := gzSHA256(filepath.Join(dir, "ocr.gz")); err == nil && got == sum {
				fmt.Printf("ocr %s %s: up to date\n", version, p)
				continue
			}
		}
		fetched = true
		data, err := download(fmt.Sprintf("%s/releases/download/%s/%s", repo, version, asset))
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		if got := hex.EncodeToString(h[:]); got != sum {
			return fmt.Errorf("%s: checksum %s, want %s", asset, got, sum)
		}
		var gz bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		if _, err := zw.Write(data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "ocr.gz"), gz.Bytes(), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "ocr.sha256"), []byte(sum+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("ocr %s %s: %.1f MB, %.1f MB compressed\n", version, p, float64(len(data))/1e6, float64(gz.Len())/1e6)
	}
	licensePath := filepath.Join(binDir, "LICENSE")
	if _, err := os.Stat(licensePath); err == nil && !fetched {
		return nil // binaries unchanged, so the license of their release is already here
	}
	license, err := download(fmt.Sprintf("https://raw.githubusercontent.com/alibaba/open-code-review/%s/LICENSE", version))
	if err != nil {
		return err
	}
	return os.WriteFile(licensePath, license, 0o600) //nolint:gosec // G703: a fixed path in the repository
}

// gzSHA256 returns the hex sha256 of a gzip file's uncompressed content.
func gzSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil { //nolint:gosec // G110: our own download, checked against its checksum
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseSums reads "<sha256>  <file>" lines.
func parseSums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

func download(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
