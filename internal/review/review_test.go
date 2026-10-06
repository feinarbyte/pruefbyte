package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/feinarbyte/pruefbyte/internal/config"
	"github.com/feinarbyte/pruefbyte/internal/gitlab"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
)

const botID = 42

// fakeGitLab keeps discussions in memory, like a tiny GitLab.
type fakeGitLab struct {
	mr          gitlab.MR
	diffs       []gitlab.FileDiff
	compare     map[string][]gitlab.FileDiff
	discussions []gitlab.Discussion
	// rejectLine makes CreateDiscussion fail with 400 for this new_line, failLine with 500.
	rejectLine, failLine int
	nextID               int64
	resolved             []string
	updates              int
}

func (f *fakeGitLab) CurrentUserID(context.Context) (int64, error)         { return botID, nil }
func (f *fakeGitLab) GetMR(context.Context) (*gitlab.MR, error)            { m := f.mr; return &m, nil }
func (f *fakeGitLab) ListDiffs(context.Context) ([]gitlab.FileDiff, error) { return f.diffs, nil }
func (f *fakeGitLab) ListDiscussions(context.Context) ([]gitlab.Discussion, error) {
	return f.discussions, nil
}

func (f *fakeGitLab) CreateDiscussion(_ context.Context, body string, pos *gitlab.Position) error {
	if pos != nil && pos.NewLine == f.rejectLine {
		return &gl.ErrorResponse{Response: &http.Response{StatusCode: 400}, Message: "400 {line_code: [can't be blank]}"}
	}
	if pos != nil && pos.NewLine == f.failLine {
		return &gl.ErrorResponse{Response: &http.Response{StatusCode: 500}, Message: "500 Internal Server Error"}
	}
	f.nextID++
	p := *pos
	f.discussions = append(f.discussions, gitlab.Discussion{
		ID:    fmt.Sprintf("d%d", f.nextID),
		Notes: []gitlab.Note{{ID: f.nextID, AuthorID: botID, Body: body, Resolvable: true, Position: &p}},
	})
	return nil
}

func (f *fakeGitLab) CreateNote(_ context.Context, body string) error {
	f.nextID++
	f.discussions = append(f.discussions, gitlab.Discussion{
		ID: fmt.Sprintf("d%d", f.nextID), IndividualNote: true,
		Notes: []gitlab.Note{{ID: f.nextID, AuthorID: botID, Body: body}},
	})
	return nil
}

func (f *fakeGitLab) UpdateNote(_ context.Context, id int64, body string) error {
	for i := range f.discussions {
		for j := range f.discussions[i].Notes {
			if f.discussions[i].Notes[j].ID == id {
				f.discussions[i].Notes[j].Body = body
				f.updates++
				return nil
			}
		}
	}
	return errors.New("note not found")
}

func (f *fakeGitLab) ReplyAndResolve(_ context.Context, id, body string) error {
	f.resolved = append(f.resolved, id)
	for i := range f.discussions {
		if f.discussions[i].ID == id {
			f.nextID++
			f.discussions[i].Notes = append(f.discussions[i].Notes, gitlab.Note{ID: f.nextID, AuthorID: botID, Body: body})
			for j := range f.discussions[i].Notes {
				f.discussions[i].Notes[j].Resolved = true
			}
		}
	}
	return nil
}

// setResolved resolves or unresolves thread i, as a person would.
func (f *fakeGitLab) setResolved(i int, resolved bool) {
	for j := range f.discussions[i].Notes {
		f.discussions[i].Notes[j].Resolved = resolved
	}
}

func (f *fakeGitLab) Compare(_ context.Context, from, to string) ([]gitlab.FileDiff, error) {
	return f.compare[from+".."+to], nil
}

func (f *fakeGitLab) inline() []gitlab.Discussion {
	var out []gitlab.Discussion
	for _, d := range f.discussions {
		if d.Notes[0].Position != nil {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeGitLab) summary() string {
	for _, d := range f.discussions {
		if strings.HasPrefix(d.Notes[0].Body, summaryMarker) {
			return d.Notes[0].Body
		}
	}
	return ""
}

type fakeOCR struct {
	res   *ocr.Result
	err   error
	opts  ocr.ReviewOptions
	calls int
	temps map[string][]byte
	// during runs inside Review, e.g. to simulate a push while OCR works.
	during func()
}

func (f *fakeOCR) Review(_ context.Context, o ocr.ReviewOptions) (*ocr.Result, []byte, error) {
	f.opts = o
	f.calls++
	if f.during != nil {
		f.during()
	}
	return f.res, []byte("{}"), f.err
}

func (f *fakeOCR) WriteTemp(name string, data []byte) (string, error) {
	if f.temps == nil {
		f.temps = map[string][]byte{}
	}
	f.temps[name] = data
	return "/tmp/" + name, nil
}

// a.go: lines 1-2 context, 3-4 added, 5 context. b.go is new.
const patchA = "@@ -1,3 +1,5 @@\n ctx1\n ctx2\n+add3\n+add4\n ctx5\n"

func newFake() *fakeGitLab {
	return &fakeGitLab{
		mr: gitlab.MR{IID: 7, Title: "Add feature", BaseSHA: "base", StartSHA: "start", HeadSHA: "head1"},
		diffs: []gitlab.FileDiff{
			{OldPath: "a.go", NewPath: "a.go", Diff: patchA},
			{OldPath: "b.go", NewPath: "b.go", NewFile: true, Diff: "@@ -0,0 +1,2 @@\n+x\n+y\n"},
		},
		compare: map[string][]gitlab.FileDiff{},
	}
}

func testConfig() config.Config {
	c := config.Default()
	c.LLM.Provider, c.LLM.Model = "anthropic", "claude-sonnet-5"
	return c
}

func deps(f *fakeGitLab, o *fakeOCR, cfg config.Config) Deps {
	return Deps{
		GitLab:     f,
		OCR:        o,
		LoadConfig: func(context.Context, string) (config.Config, error) { return cfg, nil },
		Log:        io.Discard,
	}
}

func result(cs ...ocr.Comment) *ocr.Result {
	return &ocr.Result{Status: "success", LLM: ocr.LLMInfo{Model: "claude-sonnet-5"}, Comments: cs}
}

func TestPostsInlineWithCorrectPositions(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "added line issue", StartLine: 3, EndLine: 4, Severity: "high", Category: "bug",
			ExistingCode: "add3\nadd4", SuggestionCode: "fixed3\nfixed4"},
		ocr.Comment{Path: "a.go", Content: "context line issue", StartLine: 5, EndLine: 5, Severity: "low"},
		ocr.Comment{Path: "b.go", Content: "new file issue", StartLine: 2, EndLine: 2, Severity: "medium"},
	)}
	out, err := Run(context.Background(), deps(f, o, testConfig()), Options{RepoDir: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Posted != 3 {
		t.Fatalf("posted %d, want 3", out.Stats.Posted)
	}
	if o.opts.From != "base" || o.opts.To != "head1" || o.opts.BackgroundFile == "" {
		t.Errorf("ocr options wrong: %+v", o.opts)
	}
	byPath := map[string]*gitlab.Position{}
	var first string
	for _, d := range f.inline() {
		p := d.Notes[0].Position
		byPath[fmt.Sprintf("%s:%d", p.NewPath, p.NewLine)] = p
		if strings.Contains(d.Notes[0].Body, "added line issue") {
			first = d.Notes[0].Body
		}
		if p.BaseSHA != "base" || p.StartSHA != "start" || p.HeadSHA != "head1" {
			t.Errorf("bad diff refs in %+v", p)
		}
	}
	if p := byPath["a.go:4"]; p == nil || p.OldLine != 0 {
		t.Errorf("added line: want new_line 4 without old_line, got %+v", p)
	}
	if p := byPath["a.go:5"]; p == nil || p.OldLine != 3 {
		t.Errorf("context line: want new_line 5 / old_line 3, got %+v", p)
	}
	if byPath["b.go:2"] == nil {
		t.Error("new file comment missing")
	}
	if !strings.Contains(first, "```suggestion:-1+0\nfixed3\nfixed4\n```") {
		t.Errorf("suggestion block missing:\n%s", first)
	}
	if !strings.Contains(first, "🟠 High · bug") {
		t.Errorf("badge missing:\n%s", first)
	}
	if f.summary() != "" {
		t.Error("summary note posted although everything went inline")
	}
}

func TestSecondRunPostsNoDuplicates(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "Issue  One", StartLine: 3, EndLine: 3})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	// Same finding, lines shifted, whitespace and case changed.
	o.res = result(ocr.Comment{Path: "a.go", Content: "issue one", StartLine: 4, EndLine: 4})
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Posted != 0 || out.Stats.Duplicates != 1 || len(f.inline()) != 1 {
		t.Errorf("stats %+v, %d threads", out.Stats, len(f.inline()))
	}
}

func TestOthersFingerprintsAreIgnored(t *testing.T) {
	f := newFake()
	c := ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3}
	// A human pasted the bot's marker; it must not suppress the finding.
	f.discussions = []gitlab.Discussion{{ID: "h", Notes: []gitlab.Note{{AuthorID: 1, Body: fpPrefix + fingerprint(c) + " -->"}}}}
	out, err := Run(context.Background(), deps(f, &fakeOCR{res: result(c)}, testConfig()), Options{})
	if err != nil || out.Stats.Posted != 1 {
		t.Fatalf("posted %d, err %v", out.Stats.Posted, err)
	}
}

func TestOutsideDiffAndRejectedGoToSummary(t *testing.T) {
	f := newFake()
	f.rejectLine = 2 // b.go:2
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "far away", StartLine: 40, EndLine: 41},
		ocr.Comment{Path: "untouched.go", Content: "other file", StartLine: 1, EndLine: 1},
		ocr.Comment{Path: "b.go", Content: "rejected", StartLine: 2, EndLine: 2},
	)}
	out, err := Run(context.Background(), deps(f, o, testConfig()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Unplaced != 3 || out.Stats.Posted != 0 {
		t.Fatalf("stats %+v", out.Stats)
	}
	s := f.summary()
	for _, want := range []string{"far away", "other file", "rejected", "Findings outside the diff"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary misses %q:\n%s", want, s)
		}
	}
	// A rerun updates the same note instead of adding one.
	if _, err := Run(context.Background(), deps(f, o, testConfig()), Options{}); err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, d := range f.discussions {
		if strings.HasPrefix(d.Notes[0].Body, summaryMarker) {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d summary notes, want 1", notes)
	}
}

func TestFilterCapAndGate(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.Review.MinSeverity = "medium"
	cfg.Review.MaxComments = 1
	cfg.Review.FailOnSeverity = "critical"
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "low one", StartLine: 3, EndLine: 3, Severity: "low"},
		ocr.Comment{Path: "a.go", Content: "medium one", StartLine: 4, EndLine: 4, Severity: "medium"},
		ocr.Comment{Path: "b.go", Content: "critical one", StartLine: 1, EndLine: 1, Severity: "critical"},
	)}
	out, err := Run(context.Background(), deps(f, o, cfg), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Findings != 2 || out.Stats.Posted != 1 || out.Stats.Overflow != 1 {
		t.Errorf("stats %+v", out.Stats)
	}
	if !strings.Contains(f.inline()[0].Notes[0].Body, "critical one") {
		t.Error("most severe finding should be posted first")
	}
	if !strings.Contains(f.summary(), "medium one") {
		t.Error("overflow finding should be listed in the summary")
	}
	if !out.GateFailed {
		t.Error("fail_on_severity=critical did not trip")
	}
}

func TestResolvesOnlyChangedThreads(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "will be fixed", StartLine: 3, EndLine: 3},
		ocr.Comment{Path: "a.go", Content: "code untouched", StartLine: 5, EndLine: 5},
		ocr.Comment{Path: "a.go", Content: "still there", StartLine: 4, EndLine: 4},
	)}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	// New push: line 3 of a.go changed; only "still there" is reported again.
	f.mr.HeadSHA = "head2"
	f.compare["head1..head2"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-add3\n+fix3\n"}}
	o.res = result(ocr.Comment{Path: "a.go", Content: "still there", StartLine: 4, EndLine: 4})
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Resolved != 1 || len(f.resolved) != 1 {
		t.Fatalf("resolved %v", f.resolved)
	}
	for _, disc := range f.discussions {
		n := disc.Notes[0]
		if strings.Contains(n.Body, "will be fixed") != n.Resolved {
			t.Errorf("thread %q resolved=%v", n.Body[:30], n.Resolved)
		}
	}
}

func TestNoResolveOnIncompleteReview(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.mr.HeadSHA = "head2"
	f.compare["head1..head2"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-a\n+b\n"}}
	o.res = &ocr.Result{Status: "completed_with_errors"}
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Resolved != 0 {
		t.Error("resolved threads after an incomplete review")
	}
	if !strings.Contains(f.summary(), "did not complete") {
		t.Error("incomplete review not flagged in summary")
	}
}

func TestFailurePostsRedactedNote(t *testing.T) {
	f := newFake()
	o := &fakeOCR{err: &ocr.RunError{Err: errors.New("exit status 1"), Stderr: "auth failed for key sk-secret-123"}}
	d := deps(f, o, testConfig())
	d.Redact = func(s string) string { return strings.ReplaceAll(s, "sk-secret-123", "***") }
	_, err := Run(context.Background(), d, Options{})
	if !errors.Is(err, ErrReviewFailed) {
		t.Fatalf("err = %v", err)
	}
	s := f.summary()
	if !strings.Contains(s, "failed") || strings.Contains(s, "sk-secret-123") || !strings.Contains(s, "***") {
		t.Errorf("failure note wrong:\n%s", s)
	}
}

// When to run is the CI job's decision: pruefbyte reviews whatever it is run on.
// It only skips a pipeline whose commit is no longer the MR head.
func TestSkipsOnlyStalePipelines(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3})}
	out, err := Run(context.Background(), deps(f, o, testConfig()), Options{ExpectedHeadSHA: "head1"})
	if err != nil || out.Skipped != "" || out.Stats.Posted != 1 {
		t.Fatalf("not reviewed: %+v %v", out, err)
	}
	calls := o.calls
	out, _ = Run(context.Background(), deps(f, o, testConfig()), Options{ExpectedHeadSHA: "older"})
	if out.Skipped == "" {
		t.Error("stale pipeline not skipped")
	}
	if o.calls != calls {
		t.Error("ocr ran for a stale pipeline")
	}
}

func TestSameTextOnTwoLinesIsPostedTwice(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "Error is ignored.", StartLine: 3, EndLine: 3, ExistingCode: "add3"},
		ocr.Comment{Path: "a.go", Content: "Error is ignored.", StartLine: 4, EndLine: 4, ExistingCode: "add4"},
		ocr.Comment{Path: "a.go", Content: "Error is ignored.", StartLine: 4, EndLine: 4, ExistingCode: "add4"}, // repeated
	)}
	out, err := Run(context.Background(), deps(f, o, testConfig()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Posted != 2 || len(f.inline()) != 2 {
		t.Errorf("stats %+v, %d threads", out.Stats, len(f.inline()))
	}
}

func TestRewordedFindingIsDuplicate(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "Possible nil dereference.", StartLine: 3, EndLine: 3, ExistingCode: "add3", Category: "bug"})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	o.res = result(ocr.Comment{Path: "a.go", Content: "x may be nil here.", StartLine: 3, EndLine: 3, ExistingCode: "  add3 ", Category: "Bug"})
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Posted != 0 || out.Stats.Duplicates != 1 || len(f.inline()) != 1 {
		t.Errorf("stats %+v, %d threads", out.Stats, len(f.inline()))
	}
}

func TestDifferentIssuesOnSameCodeBothPosted(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "Nil dereference.", StartLine: 3, EndLine: 3, ExistingCode: "add3", Category: "bug"},
		ocr.Comment{Path: "a.go", Content: "Error ignored.", StartLine: 3, EndLine: 3, ExistingCode: "add3", Category: "bug"},
	)}
	out, err := Run(context.Background(), deps(f, o, testConfig()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Posted != 2 {
		t.Errorf("stats %+v", out.Stats)
	}
}

func TestMaxCommentsCountsOpenThreads(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.Review.MaxComments = 1
	o := &fakeOCR{res: result(
		ocr.Comment{Path: "a.go", Content: "one", StartLine: 3, EndLine: 3, Severity: "high"},
		ocr.Comment{Path: "a.go", Content: "two", StartLine: 4, EndLine: 4, Severity: "high"},
		ocr.Comment{Path: "a.go", Content: "three", StartLine: 5, EndLine: 5, Severity: "high"},
	)}
	d := deps(f, o, cfg)
	for i := 0; i < 3; i++ { // the pipeline is retried
		if _, err := Run(context.Background(), d, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.inline()); n != 1 {
		t.Errorf("%d inline threads with max_comments=1", n)
	}
	if s := f.summary(); !strings.Contains(s, "two") || !strings.Contains(s, "three") {
		t.Errorf("overflow missing from summary:\n%s", s)
	}
}

func TestReopenedThreadIsNotResolvedAgain(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "still wrong", StartLine: 3, EndLine: 3})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.mr.HeadSHA = "head2"
	f.compare["head1..head2"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-add3\n+fix3\n"}}
	o.res = result()
	if _, err := Run(context.Background(), d, Options{}); err != nil || len(f.resolved) != 1 {
		t.Fatalf("first resolve: %v %v", f.resolved, err)
	}
	f.setResolved(0, false) // a reviewer disagrees and reopens it
	f.mr.HeadSHA = "head3"
	f.compare["head1..head3"] = f.compare["head1..head2"]
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(f.resolved) != 1 {
		t.Errorf("reopened thread resolved again: %v", f.resolved)
	}
}

func TestDiffIsReadBeforeTheReview(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "on ctx5", StartLine: 5, EndLine: 5})}
	// A push lands while OCR runs; outside CI nothing re-checks the head.
	o.during = func() {
		f.diffs = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,3 +1,7 @@\n+n1\n+n2\n ctx1\n ctx2\n+add3\n+add4\n ctx5\n"}}
	}
	if _, err := Run(context.Background(), deps(f, o, testConfig()), Options{}); err != nil {
		t.Fatal(err)
	}
	if p := f.inline()[0].Notes[0].Position; p.HeadSHA != "head1" || p.NewLine != 5 || p.OldLine != 3 {
		t.Errorf("position from the wrong diff version: %+v", p)
	}
}

func TestReRatedFindingKeepsThreadOpen(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.Review.MinSeverity = "medium"
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "Error from Close is ignored.", StartLine: 3, EndLine: 3, Severity: "medium"})}
	d := deps(f, o, cfg)
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	// Line 3 is edited without a fix; OCR reports the same finding, now as low.
	f.mr.HeadSHA = "head2"
	f.compare["head1..head2"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-add3\n+add3 // edited\n"}}
	o.res = result(ocr.Comment{Path: "a.go", Content: "Error from Close is ignored.", StartLine: 3, EndLine: 3, Severity: "low"})
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Resolved != 0 {
		t.Error("thread resolved although OCR still reports the finding")
	}
}

func TestResolveOnlyInReviewedFiles(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "a.go", Content: "x", StartLine: 3, EndLine: 3})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.mr.HeadSHA = "head2"
	f.compare["head1..head2"] = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -3 +3 @@\n-add3\n+fix3\n"}}
	// a.go grew too large and OCR left it out: complete, but a.go was not reviewed.
	o.res = &ocr.Result{Status: "complete", Manifest: &ocr.Manifest{}}
	o.res.Manifest.Coverage.Completed = []ocr.CoveredItem{{Path: "b.go"}}
	out, err := Run(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stats.Resolved != 0 {
		t.Error("resolved a thread in a file OCR did not review")
	}
}

func TestRuleFileComesFromBaseCommit(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result()}
	cfg := testConfig()
	cfg.OCR.Exclude = []string{"**/*.{pb,gen}.go"}
	d := deps(f, o, cfg)
	var reads []string
	d.ReadFileAt = func(_ context.Context, rev, path string) ([]byte, bool, error) {
		reads = append(reads, rev+":"+path)
		switch path {
		case ".opencodereview/rule.json":
			return []byte(`{"exclude": ["docs/**"], "rules": [
				{"path": "**/*.go", "rule": "base rule"},
				{"path": "web/**", "rule": "docs/web-rules.md"},
				{"path": "x/**", "rule": "../outside.md"}]}`), true, nil
		case "docs/web-rules.md":
			return []byte("web rule at base\n"), true, nil
		}
		return nil, false, nil
	}
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	rf, err := ocr.ParseRuleFile(o.temps["rule.json"])
	if err != nil || o.opts.RuleFile == "" {
		t.Fatalf("no rule file passed: %v %+v", err, o.opts)
	}
	if strings.Join(reads, " ") != "base:.opencodereview/rule.json base:docs/web-rules.md" {
		t.Errorf("reads %v", reads)
	}
	if strings.Join(rf.Exclude, ",") != "docs/**,**/*.{pb,gen}.go" || len(rf.Rules) != 4 || rf.Rules[3].Path != "**/*" {
		t.Fatalf("rule file %+v", rf)
	}
	// Rules naming a file get its content from the base commit; a path outside the
	// repository is dropped, as OCR does.
	if rf.Rules[0].Rule != "base rule" || rf.Rules[1].Rule != "web rule at base" || rf.Rules[2].Rule != "" {
		t.Errorf("rules %+v", rf.Rules)
	}
}

func TestFetchBeforeConfigBeforeOCRSetup(t *testing.T) {
	f := newFake()
	var steps []string
	d := deps(f, &fakeOCR{res: result()}, testConfig())
	d.EnsureCommits = func(context.Context, ...string) error { steps = append(steps, "fetch"); return nil }
	load := d.LoadConfig
	d.LoadConfig = func(ctx context.Context, sha string) (config.Config, error) {
		steps = append(steps, "config")
		return load(ctx, sha)
	}
	d.PrepareOCR = func(context.Context, config.Config) error { steps = append(steps, "prepare"); return nil }
	out, err := Run(context.Background(), d, Options{})
	if err != nil || out.Skipped != "" {
		t.Fatalf("run: %+v %v", out, err)
	}
	if strings.Join(steps, ",") != "fetch,config,prepare" {
		t.Errorf("steps %v", steps)
	}
	// A stale pipeline is skipped before anything is fetched or loaded.
	steps = nil
	if out, _ := Run(context.Background(), d, Options{ExpectedHeadSHA: "older"}); out.Skipped == "" || len(steps) != 0 {
		t.Errorf("stale run: %+v, steps %v", out, steps)
	}
}

func TestFailedPostIsListedInSummary(t *testing.T) {
	f := newFake()
	o := &fakeOCR{res: result(ocr.Comment{Path: "untouched.go", Content: "elsewhere", StartLine: 1, EndLine: 1})}
	d := deps(f, o, testConfig())
	if _, err := Run(context.Background(), d, Options{}); err != nil {
		t.Fatal(err)
	}
	f.failLine = 3
	o.res = result(ocr.Comment{Path: "a.go", Content: "cannot be posted", StartLine: 3, EndLine: 3})
	out, err := Run(context.Background(), d, Options{})
	if err == nil || out.Stats.Failed != 1 {
		t.Fatalf("stats %+v, err %v", out.Stats, err)
	}
	if s := f.summary(); !strings.Contains(s, "cannot be posted") || strings.Contains(s, "All findings are posted inline") {
		t.Errorf("summary hides the failed finding:\n%s", s)
	}
}
