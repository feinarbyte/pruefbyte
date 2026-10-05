package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateDiscussionPayload(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Private-Token"), r.URL.EscapedPath()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","notes":[]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/api/v4", "glpat-bot", "group/proj", 7)
	if err != nil {
		t.Fatal(err)
	}
	err = c.CreateDiscussion(context.Background(), "body", &Position{
		BaseSHA: "b", StartSHA: "s", HeadSHA: "h", OldPath: "old.go", NewPath: "new.go", NewLine: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "glpat-bot" {
		t.Errorf("PRIVATE-TOKEN header = %q", auth)
	}
	if path != "/api/v4/projects/group%2Fproj/merge_requests/7/discussions" {
		t.Errorf("path = %s", path)
	}
	pos, _ := got["position"].(map[string]any)
	want := map[string]any{"position_type": "text", "base_sha": "b", "start_sha": "s", "head_sha": "h",
		"old_path": "old.go", "new_path": "new.go", "new_line": float64(12)}
	for k, v := range want {
		if pos[k] != v {
			t.Errorf("position.%s = %v, want %v", k, pos[k], v)
		}
	}
	if _, ok := pos["old_line"]; ok {
		t.Error("old_line must be omitted for added lines")
	}
	if got["body"] != "body" {
		t.Errorf("body = %v", got["body"])
	}
}

func TestCreateIsNotResentAfter5xx(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++ // GitLab may have stored the note before failing
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"500 Internal Server Error"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL+"/api/v4", "t", "1", 1)
	if err := c.CreateDiscussion(context.Background(), "x", &Position{NewPath: "a", NewLine: 1}); err == nil {
		t.Fatal("500 reported as success")
	}
	if posts != 1 {
		t.Errorf("discussion POSTed %d times", posts)
	}
}

func TestIsBadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL+"/api/v4", "t", "1", 1)
	err := c.CreateDiscussion(context.Background(), "x", &Position{NewPath: "a", NewLine: 1})
	if !IsBadRequest(err) {
		t.Fatalf("IsBadRequest(%v) = false", err)
	}
}
