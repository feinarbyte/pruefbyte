package review

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/feinarbyte/pruefbyte/internal/config"
	"github.com/feinarbyte/pruefbyte/internal/gitlab"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
)

// covered is a complete OCR result that reviewed paths, with comments.
func covered(paths []string, cs ...ocr.Comment) *ocr.Result {
	r := result(cs...)
	r.Manifest = &ocr.Manifest{}
	for _, p := range paths {
		r.Manifest.Coverage.Selected = append(r.Manifest.Coverage.Selected, ocr.CoveredItem{Path: p})
		r.Manifest.Coverage.Completed = append(r.Manifest.Coverage.Completed, ocr.CoveredItem{Path: p})
	}
	return r
}

// reuseDeps is deps plus a diff whose keys the test controls.
func reuseDeps(f *fakeGitLab, o *fakeOCR, keys map[string]string) Deps {
	d := deps(f, o, testConfig())
	d.OCRVersion = "ocr v1"
	d.DiffKeys = func(context.Context, string, string) (map[string]string, error) { return keys, nil }
	return d
}

// excludedByRun returns the exclude patterns of the rule file OCR got last.
func excludedByRun(t *testing.T, o *fakeOCR) []string {
	t.Helper()
	var rf ocr.RuleFile
	if err := json.Unmarshal(o.temps["rule.json"], &rf); err != nil {
		t.Fatal(err)
	}
	return rf.Exclude
}

func storedState(t *testing.T, f *fakeGitLab) *ReuseState {
	t.Helper()
	return stateFromNote(f.summary())
}

func TestUnchangedFilesAreReused(t *testing.T) {
	f := newFake()
	keys := map[string]string{"a.go": "ka1", "b.go": "kb1"}
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"})}
	d := reuseDeps(f, o, keys)
	ctx := context.Background()
	if _, err := Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	if ex := excludedByRun(t, o); slices.Contains(ex, "a.go") || slices.Contains(ex, "b.go") {
		t.Fatalf("first run excluded files: %v", ex)
	}
	s := storedState(t, f)
	if s == nil || s.Files["a.go"].Key != "ka1" || s.Files["b.go"].At != "head1" {
		t.Fatalf("state not stored: %+v\n%s", s, f.summary())
	}

	// Same head, same settings: everything is excluded, so OCR selects nothing.
	o.res = covered(nil)
	o.res.Status = "skipped"
	out, err := Run(ctx, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex := excludedByRun(t, o); !slices.Contains(ex, "a.go") || !slices.Contains(ex, "b.go") {
		t.Errorf("second run did not exclude both files: %v", ex)
	}
	if out.Stats.Reused != 2 || !strings.Contains(f.summary(), "nothing was sent to the model") {
		t.Errorf("reused=%d, summary:\n%s", out.Stats.Reused, f.summary())
	}
	if s := storedState(t, f); s == nil || len(s.Files) != 2 || s.Files["a.go"].At != "head1" {
		t.Errorf("state lost after reuse: %+v", s)
	}

	// A push that changes b.go only: only a.go is excluded.
	f.mr.HeadSHA = "head2"
	keys["b.go"] = "kb2"
	o.res = covered([]string{"b.go"})
	if _, err = Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	if ex := excludedByRun(t, o); !slices.Contains(ex, "a.go") || slices.Contains(ex, "b.go") {
		t.Errorf("excludes after changing b.go: %v", ex)
	}
	if !strings.Contains(f.summary(), "1 file(s) unchanged since the review of `head1`") {
		t.Errorf("summary does not name the reused review:\n%s", f.summary())
	}
	s = storedState(t, f)
	if s.Files["a.go"].At != "head1" || s.Files["b.go"] != (ReusedFile{Key: "kb2", At: "head2"}) {
		t.Errorf("state after the push: %+v", s.Files)
	}
}

func constConfig(cfg config.Config) func(context.Context, string) (config.Config, error) {
	return func(context.Context, string) (config.Config, error) { return cfg, nil }
}

func TestSettingsChangeReviewsEverything(t *testing.T) {
	for name, change := range map[string]func(*Deps){
		"model":  func(d *Deps) { cfg := testConfig(); cfg.LLM.Model = "other"; d.LoadConfig = constConfig(cfg) },
		"effort": func(d *Deps) { cfg := testConfig(); cfg.OCR.Effort = "high"; d.LoadConfig = constConfig(cfg) },
		"rules": func(d *Deps) {
			cfg := testConfig()
			cfg.OCR.Rules = []config.Rule{{Path: "**/*.go", Rule: "No panics."}}
			d.LoadConfig = constConfig(cfg)
		},
		"exclude": func(d *Deps) {
			cfg := testConfig()
			cfg.OCR.Exclude = []string{"gen/**"}
			d.LoadConfig = constConfig(cfg)
		},
		"ocr":         func(d *Deps) { d.OCRVersion = "ocr v2" },
		"description": func(*Deps) {}, // background only: not part of the identity
	} {
		f := newFake()
		o := &fakeOCR{res: covered([]string{"a.go", "b.go"})}
		d := reuseDeps(f, o, map[string]string{"a.go": "ka", "b.go": "kb"})
		ctx := context.Background()
		if _, err := Run(ctx, d, Options{}); err != nil {
			t.Fatal(err)
		}
		f.mr.Description = "edited"
		change(&d)
		if _, err := Run(ctx, d, Options{}); err != nil {
			t.Fatal(err)
		}
		reused := slices.Contains(excludedByRun(t, o), "a.go")
		if reused != (name == "description") {
			t.Errorf("%s changed: a.go reused=%v", name, reused)
		}
	}
}

func TestFullReviewIgnoresTheState(t *testing.T) {
	f := newFake()
	keys := map[string]string{"a.go": "ka", "b.go": "kb"}
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"})}
	d := reuseDeps(f, o, keys)
	ctx := context.Background()
	if _, err := Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.mr.HeadSHA = "head2"
	if _, err := Run(ctx, d, Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if ex := excludedByRun(t, o); slices.Contains(ex, "a.go") {
		t.Errorf("--full excluded files: %v", ex)
	}
	if s := storedState(t, f); s.Files["a.go"].At != "head2" {
		t.Errorf("--full did not replace the state: %+v", s.Files)
	}
	f.mr.Labels = []string{"bug", FullReviewLabel}
	if _, err := Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	if ex := excludedByRun(t, o); slices.Contains(ex, "a.go") {
		t.Errorf("the %s label excluded files: %v", FullReviewLabel, ex)
	}
}

func TestOnlyCleanCompletedFilesAreStored(t *testing.T) {
	f := newFake()
	keys := map[string]string{"a.go": "ka", "b.go": "kb", "c.go": "kc"}
	// a.go has a finding, b.go completed cleanly, c.go failed (budget, timeout).
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"}, ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3})}
	o.res.Status = "partial"
	o.res.Manifest.Coverage.Selected = append(o.res.Manifest.Coverage.Selected, ocr.CoveredItem{Path: "c.go"})
	d := reuseDeps(f, o, keys)
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	s := storedState(t, f)
	if s == nil || len(s.Files) != 1 || s.Files["b.go"].Key != "kb" {
		t.Fatalf("want only b.go stored, got %+v", s)
	}
}

func TestThreadOnReusedFileIsLeftAlone(t *testing.T) {
	f := newFake()
	keys := map[string]string{"a.go": "ka1", "b.go": "kb1"}
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"}, ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3})}
	d := reuseDeps(f, o, keys)
	ctx := context.Background()
	if _, err := Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	// head2 changes a.go elsewhere; OCR finds nothing now, the thread's line is untouched.
	f.mr.HeadSHA, keys["a.go"] = "head2", "ka2"
	o.res = covered([]string{"a.go"})
	if out, err := Run(ctx, d, Options{}); err != nil || out.Stats.Resolved != 0 {
		t.Fatalf("resolved=%v err=%v", out, err)
	}
	if storedState(t, f).Files["a.go"].At != "head2" {
		t.Fatal("clean a.go not stored")
	}
	// head3 leaves a.go alone, so it is reused: even a change GitLab reports
	// on the thread's line must not resolve it, since a.go was not reviewed.
	f.mr.HeadSHA, keys["b.go"] = "head3", "kb3"
	f.compare["head1..head3"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-add3\n+fix3\n"}}
	o.res = covered([]string{"b.go"})
	out, err := Run(ctx, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Reused != 1 || out.Stats.Resolved != 0 || len(f.resolved) != 0 {
		t.Errorf("reused=%d resolved=%v", out.Stats.Reused, f.resolved)
	}
}

func TestFailureKeepsTheState(t *testing.T) {
	f := newFake()
	keys := map[string]string{"a.go": "ka", "b.go": "kb"}
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"})}
	d := reuseDeps(f, o, keys)
	ctx := context.Background()
	if _, err := Run(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.mr.HeadSHA, keys["b.go"] = "head2", "kb2"
	o.res, o.err = nil, errors.New("boom")
	if _, err := Run(ctx, d, Options{}); err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(f.summary(), "failed") {
		t.Fatalf("no failure note:\n%s", f.summary())
	}
	if s := storedState(t, f); s == nil || len(s.Files) != 2 {
		t.Errorf("failure note dropped the state: %+v", s)
	}
}

func TestReuseOffStoresNothing(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: covered([]string{"a.go", "b.go"})}
	d := reuseDeps(f, o, map[string]string{"a.go": "ka", "b.go": "kb"})
	cfg := testConfig()
	cfg.Review.ReuseUnchanged = false
	d.LoadConfig = constConfig(cfg)
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	if f.summary() != "" {
		t.Errorf("summary note created without anything to say:\n%s", f.summary())
	}
}

func TestPlanReuseSkipsUnsafePaths(t *testing.T) {
	keys := map[string]string{"a[1].go": "k1", "README.go": "k2", "readme.go": "k3", "ok.go": "k4"}
	prev := &ReuseState{Version: reuseVersion, Identity: "id", Files: map[string]ReusedFile{
		"a[1].go": {Key: "k1"}, "README.go": {Key: "k2"}, "readme.go": {Key: "old"}, "ok.go": {Key: "k4"},
	}}
	p := planReuse(prev, "id", keys)
	if got := p.excludes(); !slices.Equal(got, []string{"ok.go"}) {
		t.Errorf("excludes = %v, want only ok.go (glob characters; README.go would also exclude the changed readme.go)", got)
	}
	if p := planReuse(prev, "other", keys); len(p.reused) != 0 {
		t.Error("reused under another identity")
	}
}

func TestStateMarkerRoundTrip(t *testing.T) {
	s := &ReuseState{Version: reuseVersion, Identity: "id", Files: map[string]ReusedFile{"x -->.go": {Key: "k", At: "h"}}}
	body := summaryBody(summaryData{HeadSHA: "h", State: s})
	got := stateFromNote(body)
	if got == nil || got.Files["x -->.go"].Key != "k" {
		t.Fatalf("round trip: %+v\n%s", got, body)
	}
	if summaryBody(summaryData{HeadSHA: "h", State: s}) != body {
		t.Error("marker is not deterministic, so unchanged notes would be rewritten")
	}
	if stateFromNote("<!-- pruefbyte:state=!!! -->") != nil || ParseReuseState([]byte(`{"v":99,"id":"x"}`)) != nil {
		t.Error("unreadable or unknown state accepted")
	}
}
