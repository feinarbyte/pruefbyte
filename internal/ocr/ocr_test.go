package ocr

import (
	"context"
	"os"
	"path/filepath"
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
		ExtraHeaders: map[string]string{"b": "2", "a": "1", "anthropic-beta": "x-1,y-2"},
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
		"providers.anthropic.extra_headers": `a=1,anthropic-beta="x-1,y-2",b=2`,
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
		Effort: "high", Concurrency: 4, RuleFile: "/tmp/rule.json",
		BackgroundFile: "/tmp/bg.md", ExtraArgs: []string{"--no-filter"},
	}.Args("/tmp/out.json")
	got := strings.Join(args, " ")
	for _, want := range []string{
		"review --repo /repo --from base --to head --format json --audience agent --output /tmp/out.json",
		"--provider openai", "--model gpt-5", "--effort high", "--concurrency 4",
		"--rule /tmp/rule.json", "--background-file /tmp/bg.md", "--no-filter",
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
	parse := func(b []byte, err error) RuleFile {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		rf, err := ParseRuleFile(b)
		if err != nil {
			t.Fatal(err)
		}
		return rf
	}
	catchAll := config.Rule{Path: "**/*", MergeSystemRule: true}

	// Nothing configured: still a filter and a catch-all, so the checkout's
	// .opencodereview/rule.json cannot apply.
	rf := parse(RuleJSON(nil, nil))
	if len(rf.Exclude) != 1 || len(rf.Include) != 0 || len(rf.Rules) != 1 || rf.Rules[0] != catchAll {
		t.Errorf("empty rule file: %+v", rf)
	}

	// Rules keep layer order; the first layer with a filter wins, like in OCR;
	// extra excludes (brace globs included) are added to it.
	own := RuleFile{Rules: []config.Rule{{Path: "**/*.go", Rule: "check errors", MergeSystemRule: true}}}
	base := RuleFile{Include: []string{"src/**"}, Exclude: []string{"gen/**"}, Rules: []config.Rule{{Path: "**/*.ts", Rule: "ts"}}}
	rf = parse(RuleJSON([]RuleFile{own, base, {Exclude: []string{"never/**"}}}, []string{"**/*.{pb,gen}.go"}))
	if len(rf.Rules) != 3 || rf.Rules[0].Rule != "check errors" || !rf.Rules[0].MergeSystemRule || rf.Rules[1].Rule != "ts" || rf.Rules[2] != catchAll {
		t.Errorf("rules: %+v", rf.Rules)
	}
	if strings.Join(rf.Include, ",") != "src/**" || strings.Join(rf.Exclude, " ") != "gen/** **/*.{pb,gen}.go" {
		t.Errorf("filter: include %v exclude %v", rf.Include, rf.Exclude)
	}
	if strings.Join(base.Exclude, ",") != "gen/**" {
		t.Errorf("input layer modified: %v", base.Exclude)
	}
}

func TestReviewedCoverage(t *testing.T) {
	r, err := ParseResult([]byte(`{"status": "complete", "comments": [],
		"manifest": {"coverage": {"completed": [{"path": "a.go"}], "reused": [{"path": "b.go"}], "failed": [{"path": "c.go"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Reviewed("a.go") || !r.Reviewed("b.go") || r.Reviewed("c.go") || r.Reviewed("big.go") {
		t.Error("coverage not honoured")
	}
	skipped := &Result{Status: "skipped"}
	if skipped.Reviewed("a.go") || !(&Result{Status: "success"}).Reviewed("a.go") {
		t.Error("status fallback wrong")
	}
}

func TestCommandRefusesCmdMetacharacters(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "ocr.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Binary: shim, Home: dir}
	if _, err := r.command(context.Background(), "config", "set", "model", `m" & calc & rem "`); err == nil {
		t.Error("cmd.exe metacharacters accepted for a .cmd binary")
	}
	if _, err := r.command(context.Background(), "config", "set", "model", "claude-sonnet-5"); err != nil {
		t.Errorf("plain argument refused: %v", err)
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

func TestEnvIsolation(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[profile prod]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GITLAB_BOT_TOKEN", "glpat-renamed")
	t.Setenv("PRUEFBYTE_LLM_API_KEY", "sk-default")
	t.Setenv("OCR_LLM_URL", "http://elsewhere")
	r := &Runner{Home: t.TempDir(), SecretEnv: []string{"gitlab_bot_token"}}
	env := "\n" + strings.Join(r.env(), "\n") + "\n"
	for _, leaked := range []string{"GITLAB_BOT_TOKEN=", "PRUEFBYTE_LLM_API_KEY=", "OCR_LLM_URL="} {
		if strings.Contains(env, "\n"+leaked) {
			t.Errorf("%s passed to ocr", leaked)
		}
	}
	for _, want := range []string{"HOME=" + r.Home, "OCR_NO_UPDATE=1"} {
		if !strings.Contains(env, "\n"+want+"\n") {
			t.Errorf("env lacks %s", want)
		}
	}
	if _, set := os.LookupEnv("AWS_CONFIG_FILE"); !set && !strings.Contains(env, "\nAWS_CONFIG_FILE="+filepath.Join(home, ".aws", "config")+"\n") {
		t.Error("private HOME hides ~/.aws/config from the Bedrock credential chain")
	}
}

func TestExtraBodyWithNonStringKeys(t *testing.T) {
	m := settingsMap(must(ConfigSettings(config.LLM{
		Provider: "openai", Model: "gpt-5",
		ExtraBody: map[string]any{"logit_bias": map[any]any{50256: -100}},
	}, "sk", "")))
	if got := m["providers.openai.extra_body"]; got != `{"logit_bias":{"50256":-100}}` {
		t.Errorf("extra_body = %q", got)
	}
}
