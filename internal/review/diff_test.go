package review

import (
	"strings"
	"testing"

	"pruefbyte/internal/gitlab"
	"pruefbyte/internal/ocr"
)

func TestParseNewLines(t *testing.T) {
	patch := "@@ -10,4 +10,5 @@ func x() {\n a\n-b\n+B\n+C\n c\n\\ No newline at end of file\n@@ -30,2 +31,2 @@\n d\n-e\n+E\n"
	got := parseNewLines(patch)
	want := map[int]lineInfo{
		10: {oldLine: 10}, 11: {added: true}, 12: {added: true}, 13: {oldLine: 12},
		31: {oldLine: 30}, 32: {added: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("line %d: got %+v, want %+v", k, got[k], v)
		}
	}
}

func TestAnchor(t *testing.T) {
	fd := &fileDiff{lines: parseNewLines(patchA)}
	cases := []struct {
		start, end, want int
		ok               bool
	}{
		{3, 4, 4, true},
		{4, 9, 5, true}, // last diff line inside the range
		{8, 9, 0, false},
		{0, 3, 3, true},
		{3, 0, 3, true},
		{0, 0, 0, false},
	}
	for _, c := range cases {
		got, _, ok := fd.anchor(c.start, c.end)
		if got != c.want || ok != c.ok {
			t.Errorf("anchor(%d,%d) = %d,%v want %d,%v", c.start, c.end, got, ok, c.want, c.ok)
		}
	}
	unknown := &fileDiff{}
	if l, info, ok := unknown.anchor(5, 7); !ok || l != 7 || !info.added {
		t.Error("unknown patch should anchor on end line")
	}
}

func TestTouchedOldRanges(t *testing.T) {
	r := touchedOldRanges([]gitlab.FileDiff{
		{OldPath: "a.go", Diff: "@@ -3,2 +3,0 @@\n-x\n-y\n@@ -20,0 +19,1 @@\n+z\n"},
		{OldPath: "gone.go", DeletedFile: true},
		{OldPath: "new.go", NewFile: true, Diff: "@@ -0,0 +1 @@\n+n\n"},
	})
	if got := r["a.go"]; len(got) != 2 || got[0] != [2]int{3, 4} || got[1] != [2]int{20, 20} {
		t.Errorf("a.go ranges %v", got)
	}
	if len(r["gone.go"]) != 1 {
		t.Error("deleted file should count as fully touched")
	}
	if _, ok := r["new.go"]; ok {
		t.Error("new files have no old lines")
	}
}

func TestFenceAndBodies(t *testing.T) {
	c := ocr.Comment{Path: "a.go", Content: "use a fence", StartLine: 2, EndLine: 2,
		ExistingCode: "x", SuggestionCode: "```go\ny\n```"}
	body := inlineBody(c, "abc", 2, true)
	if !strings.Contains(body, "````suggestion:-0+0\n```go") {
		t.Errorf("nested fence not escaped:\n%s", body)
	}
	if extractFingerprint(body) != "abc" {
		t.Error("fingerprint not recoverable")
	}
	// Anchored on a different line than OCR's end line: no applicable suggestion.
	c.EndLine = 5
	body = inlineBody(c, "abc", 3, true)
	if strings.Contains(body, "suggestion:") || !strings.Contains(body, "Suggested change") || !strings.Contains(body, "lines 2–5") {
		t.Errorf("unexpected body:\n%s", body)
	}
	if body := inlineBody(c, "abc", 3, false); strings.Contains(body, "y\n") {
		t.Error("suggestions disabled but code rendered")
	}
}
