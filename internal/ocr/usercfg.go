package ocr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/feinarbyte/pruefbyte/internal/config"
)

// UserCredential returns the API key, or the command producing it, that the
// user's own OCR setup (~/.opencodereview/config.json) has for provider. Local
// runs use it so a developer who already runs OCR needs no extra setup. Both are
// empty when there is none.
func UserCredential(provider string) (apiKey, apiKeyCmd string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	return userCredential(filepath.Join(home, ".opencodereview", "config.json"), provider)
}

func userCredential(path, provider string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	type cred struct {
		APIKey    string `json:"api_key"`
		APIKeyCmd string `json:"api_key_cmd"`
	}
	var doc struct {
		Providers       map[string]cred `json:"providers"`
		CustomProviders map[string]cred `json:"custom_providers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return "", ""
	}
	c := doc.CustomProviders[provider]
	if config.IsBuiltinProvider(provider) {
		c = doc.Providers[provider]
	}
	return c.APIKey, c.APIKeyCmd
}

// RunKeyCommand runs an api_key_cmd the way OCR does: through the platform's
// shell (sh, or cmd.exe on Windows), with the terminal's stdin and stderr so
// prompts such as pinentry or Touch ID work, within 60 seconds, and requiring a
// single non-empty line of output.
func RunKeyCommand(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	cmd.WaitDelay = 5 * time.Second // a daemon left holding stdout must not block forever
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(out) > 64<<10 {
		return "", errors.New("output is larger than 64 KiB")
	}
	key := strings.TrimSpace(string(out))
	switch {
	case key == "":
		return "", errors.New("printed nothing")
	case strings.ContainsAny(key, "\r\n"):
		return "", errors.New("printed more than one line")
	}
	return key, nil
}
