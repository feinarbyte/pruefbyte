package ocr

import (
	"encoding/json"

	"pruefbyte/internal/config"
)

// RuleJSON renders configured rules in OCR's rule.json schema.
func RuleJSON(rules []config.Rule) ([]byte, error) {
	return json.MarshalIndent(struct {
		Rules []config.Rule `json:"rules"`
	}{rules}, "", "  ")
}
