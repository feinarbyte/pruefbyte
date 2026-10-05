package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// When FAKE_OCR is set, the test binary behaves like the ocr CLI.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_OCR") != "" {
		os.Exit(fakeOCR(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeOCR(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("open-code-review vFAKE")
		return 0
	case "config":
		// Record settings so the test can check them.
		f, _ := os.OpenFile(filepath.Join(os.Getenv("HOME"), "settings.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		defer f.Close()
		kv := args[2:]
		if len(kv) > 0 && kv[0] == "--" {
			kv = kv[1:]
		}
		fmt.Fprintf(f, "%s=%s\n", kv[0], kv[1])
		return 0
	case "review":
		var out string
		for i, a := range args {
			if a == "--output" {
				out = args[i+1]
			}
		}
		_ = os.WriteFile(os.Getenv("FAKE_OCR_ARGS"), []byte(strings.Join(args, "\n")), 0o600)
		_ = os.WriteFile(out, []byte(os.Getenv("FAKE_OCR_RESULT")), 0o600)
		return 0
	}
	return 2
}

type fakeServer struct {
	mu      sync.Mutex
	base    string
	head    string
	posted  []map[string]any
	notes   []string
	headers []string
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = append(s.headers, r.Header.Get("Private-Token"))
	w.Header().Set("Content-Type", "application/json")
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
	mrPath := "/projects/grp%2Fproj/merge_requests/1"
	switch {
	case p == "/user":
		fmt.Fprint(w, `{"id": 99, "username": "pruefbyte-bot"}`)
	case p == mrPath:
		fmt.Fprintf(w, `{"iid": 1, "title": "Feature", "description": "Adds a map", "source_branch": "feat", "target_branch": "main",
			"author": {"username": "alice"}, "draft": false, "labels": [],
			"diff_refs": {"base_sha": %q, "start_sha": %q, "head_sha": %q}}`, s.base, s.base, s.head)
	case p == mrPath+"/diffs":
		fmt.Fprint(w, `[{"old_path": "main.go", "new_path": "main.go", "diff": "@@ -1,3 +1,4 @@\n package main\n \n+var m map[string]int\n func main() {}\n"}]`)
	case p == mrPath+"/discussions" && r.Method == http.MethodGet:
		fmt.Fprint(w, `[]`)
	case p == mrPath+"/discussions" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.posted = append(s.posted, body)
		fmt.Fprint(w, `{"id": "d1", "notes": []}`)
	case p == mrPath+"/notes" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.notes = append(s.notes, fmt.Sprint(body["body"]))
		fmt.Fprint(w, `{"id": 5}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "404 Not Found"}`)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReviewEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	// Repo config at the base commit: raises min_severity so the low finding is dropped.
	os.WriteFile(filepath.Join(repo, ".pruefbyte.yml"), []byte("review:\n  min_severity: medium\nocr:\n  effort: high\n"), 0o644)
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := gitCmd(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nvar m map[string]int\nfunc main() {}\n"), 0o644)
	// Changing the config in the MR itself must have no effect.
	os.WriteFile(filepath.Join(repo, ".pruefbyte.yml"), []byte("review:\n  min_severity: low\n"), 0o644)
	gitCmd(t, repo, "commit", "-qam", "head")
	head := gitCmd(t, repo, "rev-parse", "HEAD")

	srv := &fakeServer{base: base, head: head}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	dir := t.TempDir()
	global := filepath.Join(dir, "pruefbyte.yml")
	os.WriteFile(global, []byte(fmt.Sprintf(`
llm:
  provider: anthropic
  model: claude-sonnet-5
ocr:
  binary: %q
review:
  fail_on_severity: critical
`, os.Args[0])), 0o644)

	result := `{"status": "success", "llm": {"provider": "anthropic", "model": "claude-sonnet-5"}, "comments": [
		{"path": "main.go", "content": "Writing to a nil map panics.", "start_line": 3, "end_line": 3,
		 "existing_code": "var m map[string]int", "suggestion_code": "var m = map[string]int{}", "severity": "critical", "category": "bug"},
		{"path": "main.go", "content": "Nitpick.", "start_line": 3, "end_line": 3, "severity": "low"}]}`
	argsFile := filepath.Join(dir, "args.txt")
	for k, v := range map[string]string{
		"FAKE_OCR": "1", "FAKE_OCR_RESULT": result, "FAKE_OCR_ARGS": argsFile,
		"PRUEFBYTE_GITLAB_TOKEN": "glpat-bot", "PRUEFBYTE_LLM_API_KEY": "sk-ant-test",
		"CI_SERVER_URL": ts.URL, "CI_MERGE_REQUEST_PROJECT_ID": "grp/proj", "CI_MERGE_REQUEST_IID": "1",
		"CI_COMMIT_SHA": head, "CI_PROJECT_DIR": repo, "PRUEFBYTE_ARTIFACT_DIR": filepath.Join(dir, "artifacts"),
		"CI_MERGE_REQUEST_SOURCE_BRANCH_SHA": "",
	} {
		t.Setenv(k, v)
	}

	cmd := rootCmd()
	cmd.SetArgs([]string{"review", "--config", global})
	cmd.SetOut(io.Discard)
	err := cmd.ExecuteContext(context.Background())

	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != exitGate {
		t.Fatalf("want severity gate exit, got %v", err)
	}
	if len(srv.posted) != 1 {
		t.Fatalf("posted %d discussions, want 1 (low finding filtered by repo config at base)", len(srv.posted))
	}
	pos := srv.posted[0]["position"].(map[string]any)
	if pos["new_line"] != float64(3) || pos["head_sha"] != head || pos["base_sha"] != base {
		t.Errorf("position %v", pos)
	}
	body := srv.posted[0]["body"].(string)
	if !strings.Contains(body, "suggestion:-0+0") || !strings.Contains(body, "Critical · bug") {
		t.Errorf("body:\n%s", body)
	}
	for _, h := range srv.headers {
		if h != "glpat-bot" {
			t.Fatalf("request without bot token: %q", h)
		}
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{base, head, "--effort\nhigh", "--provider\nanthropic", "--background-file"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ocr args missing %q:\n%s", want, args)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "ocr-result.json")); err != nil {
		t.Errorf("artifact not written: %v", err)
	}
}
