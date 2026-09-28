package ocr

import (
	"encoding/json"
	"fmt"
)

// Result is the document printed by `ocr review --format json`.
type Result struct {
	Status   string    `json:"status"`
	Message  string    `json:"message,omitempty"`
	LLM      LLMInfo   `json:"llm"`
	Summary  *Summary  `json:"summary,omitempty"`
	Comments []Comment `json:"comments"`
	Warnings []Warning `json:"warnings,omitempty"`
	Session  string    `json:"session_id,omitempty"`
}

type LLMInfo struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
}

type Summary struct {
	FilesReviewed int    `json:"files_reviewed"`
	Comments      int    `json:"comments"`
	TotalTokens   int    `json:"total_tokens"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	Elapsed       string `json:"elapsed"`
}

// Comment is one finding. Severity and Category are not part of OCR's documented
// schema but appear in practice; both may be empty.
type Comment struct {
	Path           string `json:"path"`
	Content        string `json:"content"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	ExistingCode   string `json:"existing_code,omitempty"`
	SuggestionCode string `json:"suggestion_code,omitempty"`
	Severity       string `json:"severity,omitempty"`
	Category       string `json:"category,omitempty"`
}

type Warning struct {
	File    string `json:"file,omitempty"`
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

func ParseResult(data []byte) (*Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing ocr JSON output: %w", err)
	}
	if r.Status == "" {
		return nil, fmt.Errorf("ocr JSON output has no status field")
	}
	return &r, nil
}

// Complete reports whether every file was reviewed, i.e. an absent finding
// means "no issue" rather than "not looked at".
func (r *Result) Complete() bool {
	switch r.Status {
	case "success", "completed_with_warnings", "complete", "skipped":
		return true
	}
	return false
}

// Failed reports whether the run produced nothing usable.
func (r *Result) Failed() bool { return r.Status == "failed" }
