package ocr

import (
	"strings"
	"testing"

	"pruefbyte/internal/config"
)

func TestParseResult(t *testing.T) {
	doc := `{
  "status": "success",
  "llm": {"provider": "anthropic", "model": "claude-sonnet-5"},
  "summary": {"files_reviewed": 2, "comments": 1, "total_tokens": 100, "input_tokens": 80, "output_tokens": 20, "elapsed": "5s"},
  "comments": [{"path": "a.go", "content": "nil deref", "start_line": 3, "end_line": 4,
    "existing_code": "x.y", "suggestion_code": "if x != nil { x.y }", "thinking": "…",
    "severity": "high", "category": "bug"}],
  "session_id": "abc"
}`
	r, err := ParseResult([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete() || r.Failed() {
		t.Errorf("status handling wrong for %q", r.Status)
	}
	c := r.Comments[0]
	if c.Path != "a.go" || c.StartLine != 3 || c.EndLine != 4 || c.Severity != "high" || c.Category != "bug" {
		t.Errorf("unexpected comment %+v", c)
	}
	if _, err := ParseResult([]byte(`{"comments": []}`)); err == nil {
		t.Error("document without status accepted")
	}
	if _, err := ParseResult([]byte(`not json`)); err == nil {
		t.Error("garbage accepted")
	}
	partial, _ := ParseResult([]byte(`{"status":"partial","comments":[]}`))
	if partial.Complete() {
		t.Error("partial counted as complete")
	}
}

func settingsMap(kv [][2]string) map[string]string {
	m := map[string]string{}
	for _, p := range kv {
		m[p[0]] = p[1]
	}
	return m
}

func TestConfigSettingsBuiltin(t *testing.T) {
	kv, err := ConfigSettings(config.LLM{
		Provider: "anthropic", Model: "claude-sonnet-5", URL: "https://proxy/v1",
		ExtraBody:    map[string]any{"thinking": map[string]any{"type": "disabled"}},
		ExtraHeaders: map[string]string{"b": "2", "a": "1"},
		TimeoutSec:   600,
	}, "sk-test", "German")
	if err != nil {
		t.Fatal(err)
	}
	m := settingsMap(kv)
	want := map[string]string{
		"provider":                          "anthropic",
		"model":                             "claude-sonnet-5",
		"providers.anthropic.api_key":       "sk-test",
		"providers.anthropic.url":           "https://proxy/v1",
		"providers.anthropic.extra_body":    `{"thinking":{"type":"disabled"}}`,
		"providers.anthropic.extra_headers": "a=1,b=2",
		"providers.anthropic.timeout_sec":   "600",
		"language":                          "German",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if kv[0][0] != "provider" {
		t.Error("provider must be set first")
	}
}

func TestConfigSettingsCustom(t *testing.T) {
	m := settingsMap(must(ConfigSettings(config.LLM{
		Provider: "ollama", Model: "qwen3:32b", URL: "http://127.0.0.1:11434/v1", Protocol: "openai",
	}, "", "")))
	if m["custom_providers.ollama.protocol"] != "openai" || m["custom_providers.ollama.model"] != "qwen3:32b" ||
		m["custom_providers.ollama.url"] == "" {
		t.Errorf("custom provider settings wrong: %v", m)
	}
	if m["custom_providers.ollama.api_key"] == "" {
		t.Error("custom providers need a placeholder api_key")
	}
	if _, ok := m["language"]; ok {
		t.Error("empty language should not be set")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestReviewArgs(t *testing.T) {
	args := ReviewOptions{
		RepoDir: "/repo", From: "base", To: "head", Provider: "openai", Model: "gpt-5",
		Effort: "high", Concurrency: 4, Exclude: []string{"a/**", "b"}, RuleFile: "/tmp/rule.json",
		BackgroundFile: "/tmp/bg.md", ExtraArgs: []string{"--no-filter"},
	}.Args("/tmp/out.json")
	got := strings.Join(args, " ")
	for _, want := range []string{
		"review --repo /repo --from base --to head --format json --audience agent --output /tmp/out.json",
		"--provider openai", "--model gpt-5", "--effort high", "--concurrency 4",
		"--exclude a/**,b", "--rule /tmp/rule.json", "--background-file /tmp/bg.md", "--no-filter",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "--max-tokens-budget") {
		t.Error("zero budget should not be passed")
	}
}

func TestRuleJSON(t *testing.T) {
	b, err := RuleJSON([]config.Rule{{Path: "**/*.go", Rule: "check errors"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"path": "**/*.go"`) || !strings.Contains(string(b), `"rule": "check errors"`) {
		t.Errorf("unexpected rule.json: %s", b)
	}
}

func TestTailBuffer(t *testing.T) {
	var tb tailBuffer
	tb.Write([]byte(strings.Repeat("x", tailSize)))
	tb.Write([]byte("END"))
	if s := tb.String(); len(s) != tailSize || !strings.HasSuffix(s, "END") {
		t.Errorf("tail buffer kept %d bytes", len(s))
	}
}
