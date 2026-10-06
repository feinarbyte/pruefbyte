package ocr

import (
	"encoding/json"
	"os"
	"path/filepath"

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
