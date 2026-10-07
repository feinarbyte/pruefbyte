package review

import (
	"strings"
	"testing"

	"github.com/feinarbyte/pruefbyte/internal/gitlab"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
)

func TestParsePatch(t *testing.T) {
	patch := "@@ -10,4 +10,5 @@ func x() {\n a\n-b\n+B\n+C\n c\n\\ No newline at end of file\n@@ -30,2 +31,2 @@\n d\n-e\n+E\n"
	got, old := parsePatch(patch)
	want := map[int]lineInfo{
		10: {oldLine: 10, text: "a"}, 11: {added: true, text: "B"}, 12: {added: true, text: "C"}, 13: {oldLine: 12, text: "c"},
		31: {oldLine: 30, text: "d"}, 32: {added: true, text: "E"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("line %d: got %+v, want %+v", k, got[k], v)
		}
	}
	wantOld := map[int]oldLineInfo{
		10: {newLine: 10, text: "a"}, 11: {removed: true, text: "b"}, 12: {newLine: 13, text: "c"},
		30: {newLine: 31, text: "d"}, 31: {removed: true, text: "e"},
	}
	if len(old) != len(wantOld) {
		t.Fatalf("old %v", old)
	}
	for k, v := range wantOld {
		if old[k] != v {
			t.Errorf("old line %d: got %+v, want %+v", k, old[k], v)
		}
	}
}

func TestPlace(t *testing.T) {
	fd := &fileDiff{}
	fd.lines, fd.oldLines = parsePatch(patchA)
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
		pl, ok := fd.place(ocr.Comment{StartLine: c.start, EndLine: c.end})
		if pl.newLine != c.want || ok != c.ok || pl.suggest {
			t.Errorf("place(%d,%d) = %+v,%v want new line %d,%v", c.start, c.end, pl, ok, c.want, c.ok)
		}
	}
	unknown := &fileDiff{}
	if pl, ok := unknown.place(ocr.Comment{StartLine: 5, EndLine: 7, ExistingCode: "x"}); !ok || pl.newLine != 7 || pl.suggest {
		t.Errorf("unknown patch should anchor on end line without a suggestion, got %+v", pl)
	}
}

func TestPlaceChecksExistingCode(t *testing.T) {
	// Old 1-5: a b check() c d; new 1-5: a b c d e.
	fd := &fileDiff{}
	fd.lines, fd.oldLines = parsePatch("@@ -1,5 +1,5 @@\n a\n b\n-check()\n c\n d\n+e\n")

	// Code on the new side: suggestion allowed.
	pl, ok := fd.place(ocr.Comment{StartLine: 2, EndLine: 3, ExistingCode: "b\n  c"})
	if !ok || pl.newLine != 3 || pl.oldLine != 4 || !pl.suggest {
		t.Errorf("new-side match: %+v", pl)
	}
	// The quoted code was removed, so OCR's numbers are old-file lines: comment on
	// the removed line, and never offer to overwrite new lines 1-3 with it.
	pl, ok = fd.place(ocr.Comment{StartLine: 1, EndLine: 3, ExistingCode: "a\nb\ncheck()"})
	if !ok || pl.newLine != 0 || pl.oldLine != 3 || pl.suggest {
		t.Errorf("old-side match: %+v", pl)
	}
	// Code that is nowhere at these lines: anchored as before, no suggestion.
	pl, ok = fd.place(ocr.Comment{StartLine: 3, EndLine: 3, ExistingCode: "other()"})
	if !ok || pl.newLine != 3 || pl.suggest {
		t.Errorf("mismatch: %+v", pl)
	}
}

func TestTouchedOldRanges(t *testing.T) {
	r := touchedOldRanges([]gitlab.FileDiff{
		{OldPath: "a.go", Diff: "@@ -3,2 +3,0 @@\n-x\n-y\n@@ -20,0 +19,1 @@\n+z\n"},
		{OldPath: "gone.go", DeletedFile: true},
		{OldPath: "new.go", NewFile: true, Diff: "@@ -0,0 +1 @@\n+n\n"},
		// GitLab puts context around a change: only old line 12 changed here.
		{OldPath: "ctx.go", Diff: "@@ -9,7 +9,7 @@\n l9\n l10\n l11\n-l12\n+L12\n l13\n l14\n l15\n"},
		// A pure insertion inside context touches the line it follows.
		{OldPath: "ins.go", Diff: "@@ -4,4 +4,5 @@\n l4\n l5\n+new\n l6\n l7\n"},
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
	if got := r["ctx.go"]; len(got) != 1 || got[0] != [2]int{12, 12} {
		t.Errorf("context lines counted as changed: %v", got)
	}
	if got := r["ins.go"]; len(got) != 1 || got[0] != [2]int{5, 5} {
		t.Errorf("insertion ranges %v", got)
	}
}

func TestFenceAndBodies(t *testing.T) {
	c := ocr.Comment{Path: "a.go", Content: "use a fence", StartLine: 2, EndLine: 2,
		ExistingCode: "x", SuggestionCode: "```go\ny\n```"}
	body := inlineBody(c, "abc", placement{newLine: 2, at: 2, suggest: true}, true)
	if !strings.Contains(body, "````suggestion:-0+0\n```go") {
		t.Errorf("nested fence not escaped:\n%s", body)
	}
	if extractFingerprint(body) != "abc" {
		t.Error("fingerprint not recoverable")
	}
	// Anchored on a different line than OCR's end line: no applicable suggestion.
	c.EndLine = 5
	body = inlineBody(c, "abc", placement{newLine: 3, at: 3}, true)
	if strings.Contains(body, "suggestion:") || !strings.Contains(body, "Suggested change") || !strings.Contains(body, "lines 2–5") {
		t.Errorf("unexpected body:\n%s", body)
	}
	if body := inlineBody(c, "abc", placement{newLine: 3, at: 3}, false); strings.Contains(body, "y\n") {
		t.Error("suggestions disabled but code rendered")
	}
}

func TestQuickActionsNeutralized(t *testing.T) {
	c := ocr.Comment{Path: "a.go", Content: "Missing check.\n/approve\n  /merge\n```sh\n/usr/bin/x\n```\nsee /tmp", StartLine: 2, EndLine: 2}
	body := inlineBody(c, "abc", placement{newLine: 2, at: 2}, true)
	for _, l := range strings.Split(body, "\n") {
		if t2 := strings.TrimLeft(l, " "); strings.HasPrefix(t2, "/approve") || strings.HasPrefix(t2, "/merge") {
			t.Errorf("quick action left in body:\n%s", body)
		}
	}
	if !strings.Contains(body, "\n/usr/bin/x\n") || !strings.Contains(body, "see /tmp") {
		t.Errorf("code or inline text changed:\n%s", body)
	}
}

func TestFailureNoteFence(t *testing.T) {
	s := summaryBody(summaryData{HeadSHA: "abc", Failure: "boom\n```json\n{}\n```\n/close"})
	if !strings.Contains(s, "````\nboom\n```json") || !strings.HasSuffix(strings.TrimSpace(s), "````\n\n</details>") {
		t.Errorf("failure text escapes its fence:\n%s", s)
	}
}
