package ocr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/feinarbyte/pruefbyte/internal/config"
)

// UserCred is what the user's own OCR setup has for a provider.
type UserCred struct {
	APIKey    string `json:"api_key"`
	APIKeyCmd string `json:"api_key_cmd"` // command printing the key
	// URL is the endpoint the key is for, when the user's setup overrides the
	// provider's default.
	URL string `json:"url"`
}

// UserCredential returns the API key, or the command producing it, that the
// user's own OCR setup (~/.opencodereview/config.json) has for provider. Local
// runs use it so a developer who already runs OCR needs no extra setup. It is
// the zero value when there is none.
func UserCredential(provider string) UserCred {
	home, err := os.UserHomeDir()
	if err != nil {
		return UserCred{}
	}
	return userCredential(filepath.Join(home, ".opencodereview", "config.json"), provider)
}

func userCredential(path, provider string) UserCred {
	data, err := os.ReadFile(path)
	if err != nil {
		return UserCred{}
	}
	var doc struct {
		Providers       map[string]UserCred `json:"providers"`
		CustomProviders map[string]UserCred `json:"custom_providers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return UserCred{}
	}
	if config.IsBuiltinProvider(provider) {
		return doc.Providers[provider]
	}
	return doc.CustomProviders[provider]
}

// RunKeyCommand runs an api_key_cmd the way OCR does: through the platform's
// shell (sh, or cmd.exe on Windows), with the terminal's stdin and stderr so
// prompts such as pinentry or Touch ID work, within 60 seconds, and requiring a
// single non-empty line of output.
func RunKeyCommand(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := shellCommand(ctx, command)
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
