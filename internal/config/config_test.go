package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLayering(t *testing.T) {
	cfg := Default()
	global := `
llm:
  provider: anthropic
  model: claude-sonnet-5
review:
  max_comments: 10
  min_severity: medium
`
	repo := `
llm:
  model: claude-opus-5-5
ocr:
  effort: high
  exclude: ["docs/**"]
review:
  min_severity: high
`
	if err := cfg.ApplyGlobal([]byte(global)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyRepo([]byte(repo)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyEnv(env(map[string]string{
		"PRUEFBYTE_REVIEW_MAX_COMMENTS": "5",
		"PRUEFBYTE_OCR_TIMEOUT":         "5m",
		"PRUEFBYTE_REVIEW_CATEGORIES":   "bug, security",
		"PRUEFBYTE_REVIEW_SUGGESTIONS":  "false",
	})); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"provider from global", cfg.LLM.Provider, "anthropic"},
		{"model from repo", cfg.LLM.Model, "claude-opus-5-5"},
		{"effort from repo", cfg.OCR.Effort, "high"},
		{"min severity from repo", cfg.Review.MinSeverity, "high"},
		{"max comments from env", cfg.Review.MaxComments, 5},
		{"timeout from env", cfg.OCR.Timeout, 5 * time.Minute},
		{"suggestions from env", cfg.Review.Suggestions, false},
		{"categories from env", strings.Join(cfg.Review.Categories, "|"), "bug|security"},
		{"default kept", cfg.GitLab.TokenEnv, "PRUEFBYTE_GITLAB_TOKEN"},
		{"exclude from repo", strings.Join(cfg.OCR.Exclude, "|"), "docs/**"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestRepoFileCannotRedirectSecrets(t *testing.T) {
	for _, doc := range []string{
		"llm:\n  url: https://evil.example/v1\n",
		"llm:\n  provider: my-proxy\n",
		"llm:\n  api_key_env: CI_JOB_TOKEN\n",
		"gitlab:\n  url: https://evil.example\n",
		"ocr:\n  binary: /bin/sh\n",
		"ocr:\n  extra_args: [--tools, x.json]\n",
	} {
		cfg := Default()
		if err := cfg.ApplyRepo([]byte(doc)); err == nil {
			t.Errorf("ApplyRepo(%q) succeeded, want error", doc)
		}
	}
}

func TestRuleMergeSystemRule(t *testing.T) {
	cfg := Default()
	doc := "ocr:\n  rules:\n    - path: \"**/*.go\"\n      rule: check errors\n      merge_system_rule: true\n"
	if err := cfg.ApplyRepo([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if len(cfg.OCR.Rules) != 1 || !cfg.OCR.Rules[0].MergeSystemRule {
		t.Errorf("rules %+v", cfg.OCR.Rules)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyGlobal([]byte("review:\n  max_coments: 3\n")); err == nil {
		t.Fatal("typo in key was accepted")
	}
}

func TestEmptyFiles(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyGlobal(nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyRepo([]byte("# only a comment\n")); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c := Default()
		c.LLM.Provider, c.LLM.Model = "openai", "gpt-5"
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"missing model":        func(c *Config) { c.LLM.Model = "" },
		"custom w/o protocol":  func(c *Config) { c.LLM.Provider = "gateway"; c.LLM.URL = "https://x" },
		"custom w/o url":       func(c *Config) { c.LLM.Provider = "gateway"; c.LLM.Protocol = "openai" },
		"bad severity":         func(c *Config) { c.Review.MinSeverity = "urgent" },
		"bad gate":             func(c *Config) { c.Review.FailOnSeverity = "bad" },
		"bad effort":           func(c *Config) { c.OCR.Effort = "max" },
		"rules and rule file":  func(c *Config) { c.OCR.RuleFile = "r.json"; c.OCR.Rules = []Rule{{Path: "**", Rule: "x"}} },
		"negative max comment": func(c *Config) { c.Review.MaxComments = -1 },
		"quote in header":      func(c *Config) { c.LLM.ExtraHeaders = map[string]string{"x-a": `a"b`} },
		"comma in header name": func(c *Config) { c.LLM.ExtraHeaders = map[string]string{"x,a": "b"} },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := base()
	c.LLM.Provider, c.LLM.Protocol, c.LLM.URL = "ollama", "openai", "http://localhost:11434/v1"
	if err := c.Validate(); err != nil {
		t.Errorf("custom provider rejected: %v", err)
	}
	// OCR presets need no protocol or url; OCR refuses them as custom providers.
	for _, p := range []string{"litellm", "mistral", "edenai", "ollama-cloud", "z-ai-coding"} {
		c := base()
		c.LLM.Provider = p
		if err := c.Validate(); err != nil || !IsBuiltinProvider(p) {
			t.Errorf("preset %s: %v", p, err)
		}
	}
	c = base()
	c.LLM.ExtraHeaders = map[string]string{"anthropic-beta": "a,b"}
	if err := c.Validate(); err != nil {
		t.Errorf("header value with commas rejected: %v", err)
	}
}

func TestEnvBadValue(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyEnv(env(map[string]string{"PRUEFBYTE_REVIEW_MAX_COMMENTS": "many"})); err == nil {
		t.Fatal("non-numeric int accepted")
	}
}

func TestRepoFileSetsBuiltinProvider(t *testing.T) {
	cfg := Default()
	if err := cfg.ApplyGlobal([]byte("llm:\n  provider: gateway\n  protocol: openai\n  url: https://gw.internal/v1\n  model: x\n")); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyRepo([]byte("llm:\n  provider: anthropic\n  model: claude-sonnet-5\n")); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Provider != "anthropic" || cfg.LLM.Model != "claude-sonnet-5" {
		t.Errorf("llm = %+v", cfg.LLM)
	}
	// The gateway's endpoint must not be used as anthropic's URL.
	if cfg.LLM.URL != "" || cfg.LLM.Protocol != "" {
		t.Errorf("endpoint of the global provider kept: %+v", cfg.LLM)
	}
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}
