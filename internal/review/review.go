// Package review runs OCR on a merge request and publishes the findings as
// GitLab diff discussions from the bot account.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// EnsureCommits makes sure the given commits exist in the local clone.
	EnsureCommits func(ctx context.Context, shas ...string) error
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
	cfg, err := d.LoadConfig(ctx, mr.BaseSHA)
	if err != nil {
		return nil, err
	}
	if reason := skipReason(cfg.Skip, mr); reason != "" {
		logf("skipping !%d: %s", mr.IID, reason)
		return &Outcome{Skipped: reason}, nil
	}
	if opts.ExpectedHeadSHA != "" && opts.ExpectedHeadSHA != mr.HeadSHA {
		reason := fmt.Sprintf("merge request head moved from %.8s to %.8s; a newer pipeline reviews it", opts.ExpectedHeadSHA, mr.HeadSHA)
		logf("skipping !%d: %s", mr.IID, reason)
		return &Outcome{Skipped: reason}, nil
	}
	if d.EnsureCommits != nil {
		if err := d.EnsureCommits(ctx, mr.BaseSHA, mr.HeadSHA); err != nil {
			return nil, err
		}
	}

	existing, err := d.GitLab.ListDiscussions(ctx)
	if err != nil {
		return nil, err
	}
	state := collectBotState(existing, botID)

	logf("reviewing !%d %s (%.8s..%.8s)", mr.IID, mr.Title, mr.BaseSHA, mr.HeadSHA)
	ro, err := reviewOptions(cfg, mr, opts.RepoDir, d.OCR)
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

	diffs, err := d.GitLab.ListDiffs(ctx)
	if err != nil {
		return nil, err
	}
	idx := buildDiffIndex(diffs)

	findings := filterFindings(res.Comments, cfg.Review)
	out := &Outcome{}
	st := &out.Stats
	st.Findings = len(findings)
	current := map[string]bool{}
	var unplaced, overflow []ocr.Comment

	for _, c := range findings {
		fp := fingerprint(c)
		if current[fp] {
			continue // the same finding twice in one run
		}
		current[fp] = true
		if state.fingerprints[fp] {
			st.Duplicates++
			continue
		}
		if cfg.Review.MaxComments > 0 && st.Posted >= cfg.Review.MaxComments {
			overflow = append(overflow, c)
			continue
		}
		fd := idx[c.Path]
		if fd == nil {
			unplaced = append(unplaced, c)
			continue
		}
		line, info, ok := fd.anchor(c.StartLine, c.EndLine)
		if !ok {
			unplaced = append(unplaced, c)
			continue
		}
		pos := &gitlab.Position{
			BaseSHA: mr.BaseSHA, StartSHA: mr.StartSHA, HeadSHA: mr.HeadSHA,
			OldPath: fd.oldPath, NewPath: c.Path, NewLine: line,
		}
		if !info.added {
			pos.OldLine = info.oldLine
		}
		err := d.GitLab.CreateDiscussion(ctx, inlineBody(c, fp, line, cfg.Review.Suggestions), pos)
		switch {
		case err == nil:
			st.Posted++
		case gitlab.IsBadRequest(err):
			logf("GitLab rejected the position %s:%d, listing it in the summary: %v", c.Path, line, err)
			unplaced = append(unplaced, c)
		default:
			logf("warning: posting comment on %s:%d: %v", c.Path, line, err)
			st.Failed++
		}
	}
	st.Unplaced, st.Overflow = len(unplaced), len(overflow)

	if cfg.Review.ResolveOutdated {
		if res.Complete() && st.Failed == 0 {
			st.Resolved = resolveOutdated(ctx, d.GitLab, state, current, idx, mr.HeadSHA, logf)
		} else {
			logf("not resolving earlier threads: this review was incomplete")
		}
	}

	summary := summaryData{
		Model: res.LLM.Model, HeadSHA: mr.HeadSHA, Stats: *st,
		Unplaced: unplaced, Overflow: overflow, MaxComments: cfg.Review.MaxComments,
		Warnings: res.Warnings, Incomplete: !res.Complete(),
	}
	if len(unplaced)+len(overflow)+len(res.Warnings) > 0 || !res.Complete() || state.summary != nil {
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

func reviewOptions(cfg config.Config, mr *gitlab.MR, repoDir string, r Reviewer) (ocr.ReviewOptions, error) {
	ro := ocr.ReviewOptions{
		RepoDir: repoDir, From: mr.BaseSHA, To: mr.HeadSHA,
		Provider: cfg.LLM.Provider, Model: cfg.LLM.Model,
		Effort: cfg.OCR.Effort, Concurrency: cfg.OCR.Concurrency,
		MaxTokensBudget: cfg.OCR.MaxTokensBudget, Exclude: cfg.OCR.Exclude,
		RuleFile: cfg.OCR.RuleFile, ExtraArgs: cfg.OCR.ExtraArgs,
	}
	if len(cfg.OCR.Rules) > 0 {
		data, err := ocr.RuleJSON(cfg.OCR.Rules)
		if err != nil {
			return ro, err
		}
		if ro.RuleFile, err = r.WriteTemp("rule.json", data); err != nil {
			return ro, err
		}
	}
	if cfg.OCR.UseMRDescriptionAsBackground {
		bg := "# " + mr.Title + "\n\n" + mr.Description
		if r := []rune(bg); len(r) > 8000 {
			bg = string(r[:8000])
		}
		p, err := r.WriteTemp("background.md", []byte(bg))
		if err != nil {
			return ro, err
		}
		ro.BackgroundFile = p
	}
	return ro, nil
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
		if strings.HasPrefix(first.Body, summaryMarker) {
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
			s.open = append(s.open, botThread{discussionID: d.ID, fingerprint: fp, pos: first.Position})
		}
	}
	return s
}

// resolveOutdated resolves the bot's open threads whose finding was not reported
// again and whose code changed since the comment was made. Requiring the change
// keeps an LLM that merely missed the issue this time from closing the thread.
func resolveOutdated(ctx context.Context, api gitlab.API, s botState, current map[string]bool, idx diffIndex, headSHA string, logf func(string, ...any)) int {
	resolved := 0
	compares := map[string]map[string][][2]int{}
	for _, t := range s.open {
		if current[t.fingerprint] || t.pos.HeadSHA == headSHA {
			continue
		}
		changed := false
		if idx[t.pos.NewPath] == nil {
			changed = true // the file is no longer part of the MR
		} else {
			ranges, ok := compares[t.pos.HeadSHA]
			if !ok {
				diffs, err := api.Compare(ctx, t.pos.HeadSHA, headSHA)
				if err != nil {
					logf("warning: %v", err)
				}
				ranges = touchedOldRanges(diffs)
				compares[t.pos.HeadSHA] = ranges
			}
			line := t.pos.NewLine
			if line == 0 {
				line = t.pos.OldLine
			}
			for _, r := range ranges[t.pos.NewPath] {
				if line >= r[0] && line <= r[1] {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		msg := fmt.Sprintf("Resolved automatically: the code changed in `%.8s` and this finding was not reported again.", headSHA)
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
