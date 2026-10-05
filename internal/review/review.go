// Package review runs OCR on a merge request and publishes the findings as
// GitLab diff discussions from the bot account.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"pruefbyte/internal/config"
	"pruefbyte/internal/gitlab"
	"pruefbyte/internal/ocr"
)

// Reviewer runs the review engine. *ocr.Runner implements it.
type Reviewer interface {
	Review(ctx context.Context, o ocr.ReviewOptions) (*ocr.Result, []byte, error)
	WriteTemp(name string, data []byte) (string, error)
}

type Deps struct {
	GitLab gitlab.API
	OCR    Reviewer
	// LoadConfig returns the effective config, given the MR's base commit
	// (where the repository's config file is read from).
	LoadConfig func(ctx context.Context, baseSHA string) (config.Config, error)
	// PrepareOCR optionally configures the review engine.
	PrepareOCR func(ctx context.Context, cfg config.Config) error
	// EnsureCommits makes sure the given commits exist in the local clone.
	EnsureCommits func(ctx context.Context, shas ...string) error
	// ReadFileAt returns path at commit rev, or found=false. Rule files OCR would
	// read from the merge request's checkout are taken from the base commit instead.
	ReadFileAt func(ctx context.Context, rev, path string) (data []byte, found bool, err error)
	// SaveResult optionally keeps OCR's raw JSON, e.g. as a CI artifact.
	SaveResult func(raw []byte) error
	// Redact masks secrets in text that gets posted to the MR.
	Redact func(string) string
	Log    io.Writer
}

type Options struct {
	RepoDir string
	// ExpectedHeadSHA is the commit the pipeline was started for. If the MR has
	// moved on since, the run is skipped: a newer pipeline will review it.
	ExpectedHeadSHA string
}

type Stats struct {
	Findings   int
	Posted     int
	Duplicates int
	Unplaced   int
	Overflow   int
	Failed     int
	Resolved   int
}

type Outcome struct {
	Skipped    string
	Stats      Stats
	GateFailed bool
	// GateReason explains GateFailed.
	GateReason string
}

var ErrReviewFailed = errors.New("review failed")

func Run(ctx context.Context, d Deps, opts Options) (*Outcome, error) {
	logf := func(format string, a ...any) { fmt.Fprintf(d.Log, "[pruefbyte] "+format+"\n", a...) }
	skip := func(mr *gitlab.MR, reason string) (*Outcome, error) {
		logf("skipping !%d: %s", mr.IID, reason)
		return &Outcome{Skipped: reason}, nil
	}

	botID, err := d.GitLab.CurrentUserID(ctx)
	if err != nil {
		return nil, err
	}
	mr, err := d.GitLab.GetMR(ctx)
	if err != nil {
		return nil, err
	}
	if mr.BaseSHA == "" || mr.HeadSHA == "" {
		return nil, fmt.Errorf("merge request !%d has no diff refs yet; retry once GitLab has computed the diff", mr.IID)
	}
	if opts.ExpectedHeadSHA != "" && opts.ExpectedHeadSHA != mr.HeadSHA {
		return skip(mr, fmt.Sprintf("merge request head moved from %.8s to %.8s; a newer pipeline reviews it", opts.ExpectedHeadSHA, mr.HeadSHA))
	}
	// The repository config is read from the base commit, so fetch before loading it.
	if d.EnsureCommits != nil {
		if err := d.EnsureCommits(ctx, mr.BaseSHA, mr.HeadSHA); err != nil {
			return nil, err
		}
	}
	cfg, err := d.LoadConfig(ctx, mr.BaseSHA)
	if err != nil {
		return nil, err
	}
	if d.PrepareOCR != nil {
		if err := d.PrepareOCR(ctx, cfg); err != nil {
			return nil, err
		}
	}

	existing, err := d.GitLab.ListDiscussions(ctx)
	if err != nil {
		return nil, err
	}
	state := collectBotState(existing, botID)
	// Read the diff now: positions carry the SHAs read above, and once OCR is done
	// GitLab may already list the diff of a newer push. (Positions on an older
	// version are fine as long as their lines are typed from that version.)
	diffs, err := d.GitLab.ListDiffs(ctx)
	if err != nil {
		return nil, err
	}
	idx := buildDiffIndex(diffs)

	logf("reviewing !%d %s (%.8s..%.8s)", mr.IID, mr.Title, mr.BaseSHA, mr.HeadSHA)
	ro, err := reviewOptions(ctx, cfg, mr, opts.RepoDir, d)
	if err != nil {
		return nil, err
	}
	reviewCtx, cancel := context.WithTimeout(ctx, cfg.OCR.Timeout)
	res, raw, err := d.OCR.Review(reviewCtx, ro)
	cancel()
	if raw != nil && d.SaveResult != nil {
		if serr := d.SaveResult(raw); serr != nil {
			logf("warning: saving OCR result: %v", serr)
		}
	}
	if err == nil && res.Failed() {
		err = fmt.Errorf("ocr reported status %q: %s", res.Status, res.Message)
	}
	if err != nil {
		if cfg.Review.PostFailures {
			msg := err.Error()
			var re *ocr.RunError
			if errors.As(err, &re) {
				msg = re.Err.Error() + "\n\n" + lastLines(re.Stderr, 30)
			}
			if d.Redact != nil {
				msg = d.Redact(msg)
			}
			if perr := upsertSummary(ctx, d.GitLab, state, summaryBody(summaryData{HeadSHA: mr.HeadSHA, Failure: msg})); perr != nil {
				logf("warning: posting failure note: %v", perr)
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrReviewFailed, err)
	}
	logf("ocr finished with status %s: %d finding(s)", res.Status, len(res.Comments))

	findings := filterFindings(res.Comments, cfg.Review)
	out := &Outcome{}
	st := &out.Stats
	st.Findings = len(findings)
	// A finding OCR reports again keeps its thread open, even when it now falls
	// below min_severity or outside review.categories.
	current := map[string]bool{}
	for _, c := range res.Comments {
		current[fingerprint(c)] = true
	}
	seen := map[string]bool{}
	var unplaced, overflow, failed []ocr.Comment

	for _, c := range findings {
		fp := fingerprint(c)
		// Two findings share a fingerprint when they quote the same code; only an
		// identical report at the same place is the same finding twice in one run.
		key := fmt.Sprintf("%s:%d-%d:%s", fp, c.StartLine, c.EndLine, strings.ToLower(strings.Join(strings.Fields(c.Content), " ")))
		if seen[key] {
			continue
		}
		seen[key] = true
		if state.fingerprints[fp] {
			st.Duplicates++
			continue
		}
		// The limit is for the MR: the bot's open threads from earlier runs count.
		if cfg.Review.MaxComments > 0 && len(state.open)+st.Posted >= cfg.Review.MaxComments {
			overflow = append(overflow, c)
			continue
		}
		fd := idx[c.Path]
		if fd == nil {
			unplaced = append(unplaced, c)
			continue
		}
		pl, ok := fd.place(c)
		if !ok {
			unplaced = append(unplaced, c)
			continue
		}
		pos := &gitlab.Position{
			BaseSHA: mr.BaseSHA, StartSHA: mr.StartSHA, HeadSHA: mr.HeadSHA,
			OldPath: fd.oldPath, NewPath: c.Path, NewLine: pl.newLine, OldLine: pl.oldLine,
		}
		err := d.GitLab.CreateDiscussion(ctx, inlineBody(c, fp, pl, cfg.Review.Suggestions), pos)
		switch {
		case err == nil:
			st.Posted++
		case gitlab.IsBadRequest(err):
			logf("GitLab rejected the position %s:%d, listing it in the summary: %v", c.Path, pl.line(), err)
			unplaced = append(unplaced, c)
		default:
			logf("warning: posting comment on %s:%d: %v", c.Path, pl.line(), err)
			failed = append(failed, c)
		}
	}
	st.Unplaced, st.Overflow, st.Failed = len(unplaced), len(overflow), len(failed)

	if cfg.Review.ResolveOutdated {
		if res.Complete() && st.Failed == 0 {
			st.Resolved = resolveOutdated(ctx, d.GitLab, state, current, idx, res, mr.HeadSHA, logf)
		} else {
			logf("not resolving earlier threads: this review was incomplete")
		}
	}

	summary := summaryData{
		Model: res.LLM.Model, HeadSHA: mr.HeadSHA, Stats: *st,
		Unplaced: unplaced, Overflow: overflow, Failed: failed, MaxComments: cfg.Review.MaxComments,
		Warnings: res.Warnings, Incomplete: !res.Complete(),
	}
	if len(unplaced)+len(overflow)+len(failed)+len(res.Warnings) > 0 || !res.Complete() || state.summary != nil {
		if err := upsertSummary(ctx, d.GitLab, state, summaryBody(summary)); err != nil {
			logf("warning: posting summary note: %v", err)
		}
	}

	if cfg.Review.FailOnSeverity != "" {
		gate := config.SeverityRank(cfg.Review.FailOnSeverity)
		for _, c := range findings {
			if config.SeverityRank(c.Severity) >= gate {
				out.GateFailed = true
				out.GateReason = fmt.Sprintf("%s finding in %s (%s): review.fail_on_severity is %s",
					strings.ToLower(c.Severity), c.Path, lineLabel(c), cfg.Review.FailOnSeverity)
				break
			}
		}
	}
	if st.Failed > 0 {
		return out, fmt.Errorf("%d comment(s) could not be posted", st.Failed)
	}
	return out, nil
}

func reviewOptions(ctx context.Context, cfg config.Config, mr *gitlab.MR, repoDir string, d Deps) (ocr.ReviewOptions, error) {
	ro := ocr.ReviewOptions{
		RepoDir: repoDir, From: mr.BaseSHA, To: mr.HeadSHA,
		Provider: cfg.LLM.Provider, Model: cfg.LLM.Model,
		Effort: cfg.OCR.Effort, Concurrency: cfg.OCR.Concurrency,
		MaxTokensBudget: cfg.OCR.MaxTokensBudget, ExtraArgs: cfg.OCR.ExtraArgs,
	}
	// OCR reads rule files, and the files their rules name, from the checkout,
	// which the merge request controls. Like .pruefbyte.yml they come from the base
	// commit, merged into one --rule file that also keeps the checkout's
	// .opencodereview/rule.json from applying.
	atBase := func(dir string) func(string) ([]byte, bool, error) {
		return func(name string) ([]byte, bool, error) {
			p := path.Join(dir, filepath.ToSlash(name))
			if filepath.IsAbs(name) || path.IsAbs(filepath.ToSlash(name)) || p == ".." || strings.HasPrefix(p, "../") {
				return nil, false, nil // outside the repository, where OCR does not read either
			}
			if d.ReadFileAt == nil {
				return readFile(filepath.Join(repoDir, filepath.FromSlash(p)))
			}
			return d.ReadFileAt(ctx, mr.BaseSHA, p)
		}
	}
	layers := []ocr.RuleFile{{Rules: cfg.OCR.Rules}}
	for _, p := range []string{cfg.OCR.RuleFile, ".opencodereview/rule.json"} {
		if p == "" {
			continue
		}
		// An absolute ocr.rule_file lives on the runner; so do the files it names.
		read := atBase(path.Dir(filepath.ToSlash(p)))
		if filepath.IsAbs(p) {
			read = func(name string) ([]byte, bool, error) {
				if !filepath.IsAbs(name) {
					name = filepath.Join(filepath.Dir(p), name)
				}
				return readFile(name)
			}
		}
		data, found, err := read(filepath.Base(p))
		if err != nil {
			return ro, fmt.Errorf("reading %s: %w", p, err)
		}
		if !found {
			if p == cfg.OCR.RuleFile {
				return ro, fmt.Errorf("ocr.rule_file %s not found (relative paths are read at the base commit %.8s)", p, mr.BaseSHA)
			}
			continue
		}
		rf, err := ocr.ParseRuleFile(data)
		if err == nil {
			if p != cfg.OCR.RuleFile {
				read = atBase(".") // the project file's rule files are relative to the repository root
			}
			err = rf.InlineFiles(read)
		}
		if err != nil {
			return ro, fmt.Errorf("%s: %w", p, err)
		}
		layers = append(layers, rf)
	}
	data, err := ocr.RuleJSON(layers, cfg.OCR.Exclude)
	if err != nil {
		return ro, err
	}
	if ro.RuleFile, err = d.OCR.WriteTemp("rule.json", data); err != nil {
		return ro, err
	}
	if cfg.OCR.UseMRDescriptionAsBackground {
		bg := "# " + mr.Title + "\n\n" + mr.Description
		if runes := []rune(bg); len(runes) > 8000 {
			bg = string(runes[:8000])
		}
		p, err := d.OCR.WriteTemp("background.md", []byte(bg))
		if err != nil {
			return ro, err
		}
		ro.BackgroundFile = p
	}
	return ro, nil
}

// readFile is os.ReadFile with found=false for a missing file.
func readFile(name string) ([]byte, bool, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

type botState struct {
	fingerprints map[string]bool
	open         []botThread
	summary      *gitlab.Note
}

type botThread struct {
	discussionID string
	fingerprint  string
	pos          *gitlab.Position
	// reopened is set when the bot resolved the thread before and someone
	// unresolved it; it is left alone from then on.
	reopened bool
}

// collectBotState finds everything the bot posted earlier: fingerprints (resolved
// or not, so a dismissed finding is not raised again), open threads, and the summary note.
func collectBotState(ds []gitlab.Discussion, botID int64) botState {
	s := botState{fingerprints: map[string]bool{}}
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if first.AuthorID != botID || first.System {
			continue
		}
		// The summary is a plain note; an inline finding could start with the marker text.
		if first.Position == nil && strings.HasPrefix(first.Body, summaryMarker) {
			n := first
			s.summary = &n
			continue
		}
		fp := extractFingerprint(first.Body)
		if fp == "" {
			continue
		}
		s.fingerprints[fp] = true
		if first.Resolvable && !first.Resolved && first.Position != nil {
			s.open = append(s.open, botThread{discussionID: d.ID, fingerprint: fp, pos: first.Position,
				reopened: autoResolvedBefore(d.Notes[1:], botID)})
		}
	}
	return s
}

const autoResolvedPrefix = "Resolved automatically:"

func autoResolvedBefore(replies []gitlab.Note, botID int64) bool {
	for _, n := range replies {
		if n.AuthorID == botID && strings.HasPrefix(n.Body, autoResolvedPrefix) {
			return true
		}
	}
	return false
}

// resolveOutdated resolves the bot's open threads whose finding was not reported
// again, in a file this review covered, and whose code changed since the comment
// was made. Requiring the change keeps an LLM that merely missed the issue this
// time from closing the thread.
func resolveOutdated(ctx context.Context, api gitlab.API, s botState, current map[string]bool, idx diffIndex, res *ocr.Result, headSHA string, logf func(string, ...any)) int {
	resolved := 0
	compares := map[string]map[string][][2]int{}
	for _, t := range s.open {
		if current[t.fingerprint] || t.reopened || t.pos.HeadSHA == headSHA {
			continue
		}
		changed := false
		switch {
		case idx[t.pos.NewPath] == nil:
			changed = true // the file is no longer part of the MR
		case !res.Reviewed(t.pos.NewPath):
			continue // OCR did not look at the file this time
		case t.pos.NewLine == 0:
			// A comment on removed code that GitLab no longer traces to the current diff.
			changed = true
		default:
			ranges, ok := compares[t.pos.HeadSHA]
			if !ok {
				diffs, err := api.Compare(ctx, t.pos.HeadSHA, headSHA)
				if err != nil {
					logf("warning: %v", err)
				}
				ranges = touchedOldRanges(diffs)
				compares[t.pos.HeadSHA] = ranges
			}
			for _, r := range ranges[t.pos.NewPath] {
				if t.pos.NewLine >= r[0] && t.pos.NewLine <= r[1] {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		msg := fmt.Sprintf("%s the code changed in `%.8s` and this finding was not reported again.", autoResolvedPrefix, headSHA)
		if err := api.ReplyAndResolve(ctx, t.discussionID, msg); err != nil {
			logf("warning: resolving discussion %s: %v", t.discussionID, err)
			continue
		}
		resolved++
	}
	return resolved
}

func upsertSummary(ctx context.Context, api gitlab.API, s botState, body string) error {
	if s.summary != nil {
		if s.summary.Body == body {
			return nil
		}
		return api.UpdateNote(ctx, s.summary.ID, body)
	}
	return api.CreateNote(ctx, body)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
