package main

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feinarbyte/pruefbyte/internal/gitutil"
)

// TestLocalReviewsLikeCI runs the same branch through `review` (CI) and `local`
// and checks that OCR gets the same arguments, rules and provider settings.
func TestLocalReviewsLikeCI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	// Provider and model live in the repository, next to the review settings.
	os.WriteFile(filepath.Join(repo, ".pruefbyte.yml"), []byte(`llm:
  provider: anthropic
  model: claude-sonnet-5
ocr:
  effort: high
  exclude: ["gen/**"]
  rules:
    - path: "**/*.go"
      rule: "Wrap errors with %w."
review:
  min_severity: medium
  fail_on_severity: critical
`), 0o644)
	os.MkdirAll(filepath.Join(repo, ".opencodereview"), 0o755)
	os.WriteFile(filepath.Join(repo, ".opencodereview", "rule.json"), []byte(`{"rules":[{"path":"**/*.md","rule":"Docs in English."}]}`), 0o644)
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := gitCmd(t, repo, "rev-parse", "HEAD")
	gitCmd(t, repo, "switch", "-qc", "feature")
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nvar m map[string]int\nfunc main() {}\n"), 0o644)
	gitCmd(t, repo, "commit", "-qam", "Add a map")
	head := gitCmd(t, repo, "rev-parse", "HEAD")

	srv := &fakeServer{base: base, head: head}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	dir := t.TempDir()
	// The global config only says where ocr is; everything else comes from the repo.
	global := filepath.Join(dir, "pruefbyte.yml")
	os.WriteFile(global, []byte(fmt.Sprintf("ocr:\n  binary: %q\n", os.Args[0])), 0o644)
	result := `{"status": "success", "llm": {"provider": "anthropic", "model": "claude-sonnet-5"}, "comments": [
		{"path": "main.go", "content": "Writing to a nil map panics.", "start_line": 3, "end_line": 3,
		 "existing_code": "var m map[string]int", "suggestion_code": "var m = map[string]int{}", "severity": "critical", "category": "bug"},
		{"path": "main.go", "content": "Nitpick.", "start_line": 3, "end_line": 3, "severity": "low"}]}`
	for k, v := range map[string]string{
		"FAKE_OCR": "1", "FAKE_OCR_RESULT": result, "FAKE_OCR_ARGS": filepath.Join(dir, "args.txt"),
		"PRUEFBYTE_LLM_API_KEY": "sk-ant-test", "PRUEFBYTE_GITLAB_TOKEN": "glpat-bot",
	} {
		t.Setenv(k, v)
	}
	ciEnv := map[string]string{
		"CI_SERVER_URL": ts.URL, "CI_MERGE_REQUEST_PROJECT_ID": "grp/proj", "CI_MERGE_REQUEST_IID": "1",
		"CI_COMMIT_SHA": head, "CI_PROJECT_DIR": repo, "CI_MERGE_REQUEST_SOURCE_BRANCH_SHA": "",
	}
	type capture struct{ args, rule, settings string }
	run := func(name string, args ...string) (capture, string, error) {
		t.Helper()
		prefix := filepath.Join(dir, name)
		t.Setenv("FAKE_OCR_CAPTURE", prefix)
		var stdout strings.Builder
		cmd := rootCmd()
		cmd.SetArgs(append(args, "--config", global))
		cmd.SetOut(&stdout)
		err := cmd.ExecuteContext(context.Background())
		a, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
		r, _ := os.ReadFile(prefix + ".rule.json")
		s, _ := os.ReadFile(prefix + ".settings.txt")
		return capture{string(a), string(r), string(s)}, stdout.String(), err
	}

	for k, v := range ciEnv {
		t.Setenv(k, v)
	}
	ciCap, _, err := run("ci", "review")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != exitGate {
		t.Fatalf("CI run: want gate exit, got %v", err)
	}
	for k := range ciEnv {
		t.Setenv(k, "") // a developer's machine has no CI variables
	}
	localCap, stdout, err := run("local", "local", "--repo", repo, "--target", "main", "--committed")
	if !errors.As(err, &ec) || ec.code != exitGate {
		t.Fatalf("local run: want the same gate exit as CI, got %v", err)
	}

	// Paths of per-run temp files differ; everything else must be identical.
	normalize := func(args string) string {
		parts := strings.Split(args, "\n")
		for i := 1; i < len(parts); i++ {
			switch parts[i-1] {
			case "--output", "--rule", "--background-file":
				parts[i] = "<tmp>"
			}
		}
		return strings.Join(parts, "\n")
	}
	if a, b := normalize(ciCap.args), normalize(localCap.args); a != b {
		t.Errorf("ocr arguments differ\nCI:\n%s\nlocal:\n%s", a, b)
	}
	if ciCap.rule == "" || ciCap.rule != localCap.rule {
		t.Errorf("rule files differ\nCI:\n%s\nlocal:\n%s", ciCap.rule, localCap.rule)
	}
	if !strings.Contains(ciCap.rule, "Wrap errors") || !strings.Contains(ciCap.rule, "Docs in English") {
		t.Errorf("rules not merged:\n%s", ciCap.rule)
	}
	if ciCap.settings == "" || ciCap.settings != localCap.settings {
		t.Errorf("ocr settings differ\nCI:\n%s\nlocal:\n%s", ciCap.settings, localCap.settings)
	}
	for _, want := range []string{"--from\n" + base, "--to\n" + head, "--provider\nanthropic", "--model\nclaude-sonnet-5", "--effort\nhigh"} {
		if !strings.Contains(localCap.args, want) {
			t.Errorf("local args missing %q:\n%s", want, localCap.args)
		}
	}
	// Same filtering as CI: the low finding is below min_severity.
	for _, want := range []string{"main.go:3", "Critical · bug", "Writing to a nil map panics.", "1 more below review.min_severity", "CI would fail"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("local output misses %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "Nitpick") {
		t.Errorf("filtered finding printed:\n%s", stdout)
	}

	// By default uncommitted work is reviewed too, without touching the repository.
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nvar m = map[string]int{}\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "new.go"), []byte("package main\n"), 0o644)
	status := gitCmd(t, repo, "status", "--porcelain")
	wipCap, _, _ := run("wip", "local", "--repo", repo, "--target", "main")
	to := ""
	parts := strings.Split(wipCap.args, "\n")
	for i, p := range parts {
		if p == "--to" && i+1 < len(parts) {
			to = parts[i+1]
		}
	}
	if to == "" || to == head {
		t.Fatalf("working tree not reviewed: --to %q", to)
	}
	if got := gitCmd(t, repo, "show", to+":new.go"); got != "package main" {
		t.Errorf("untracked file missing from the reviewed snapshot: %q", got)
	}
	if gitCmd(t, repo, "rev-parse", "HEAD") != head || gitCmd(t, repo, "status", "--porcelain") != status {
		t.Error("local review changed the repository")
	}
}

// A provider pinned by env var, like one in the global config, cannot be
// switched by the repository; its llm.model would not fit the other provider.
func TestRepoProviderVersusEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, ".pruefbyte.yml"), []byte("llm:\n  provider: anthropic\n  model: claude-sonnet-5\n"), 0o644)
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := gitCmd(t, repo, "rev-parse", "HEAD")
	load := configLoader(&globalFlags{repoConfig: true}, gitutil.Repo{Dir: repo}, false)

	t.Setenv("PRUEFBYTE_LLM_PROVIDER", "anthropic")
	if _, err := load(context.Background(), base); err != nil {
		t.Errorf("same provider rejected: %v", err)
	}
	t.Setenv("PRUEFBYTE_LLM_PROVIDER", "openai")
	if _, err := load(context.Background(), base); err == nil || !strings.Contains(err.Error(), "PRUEFBYTE_LLM_PROVIDER") {
		t.Errorf("env provider silently replaced the repository's: %v", err)
	}
}
