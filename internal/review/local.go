package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/feinarbyte/pruefbyte/internal/gitlab"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
)

// LocalTarget is what a local run reviews: the changes from BaseSHA (where the
// merge request would branch off) to HeadSHA. Title and Description stand in for
// the merge request's, which OCR gets as background.
type LocalTarget struct {
	BaseSHA, HeadSHA   string
	Title, Description string
}

type LocalOutcome struct {
	Result *ocr.Result
	// Findings are what CI would publish, after review.min_severity and
	// review.categories, most severe first.
	Findings   []ocr.Comment
	GateReason string
}

// Local reviews a target the way Run reviews a merge request, using the same
// config (read at the base commit), rule files and filters, but publishes
// nothing. d.GitLab is not used.
func Local(ctx context.Context, d Deps, t LocalTarget, repoDir string) (*LocalOutcome, error) {
	cfg, err := d.LoadConfig(ctx, t.BaseSHA)
	if err != nil {
		return nil, err
	}
	if d.PrepareOCR != nil {
		if err := d.PrepareOCR(ctx, cfg); err != nil {
			return nil, err
		}
	}
	mr := &gitlab.MR{BaseSHA: t.BaseSHA, StartSHA: t.BaseSHA, HeadSHA: t.HeadSHA, Title: t.Title, Description: t.Description}
	ro, err := reviewOptions(ctx, cfg, mr, repoDir, d)
	if err != nil {
		return nil, err
	}
	res, err := runOCR(ctx, d, ro, cfg.OCR.Timeout)
	if err != nil {
		// ocr's output has already streamed to d.Log; repeating it would only add noise.
		var re *ocr.RunError
		if errors.As(err, &re) {
			err = fmt.Errorf("%w (see ocr's output above)", re.Err)
		}
		return nil, fmt.Errorf("%w: %w", ErrReviewFailed, err)
	}
	// CI posts a finding OCR reports twice in one run only once.
	var findings []ocr.Comment
	seen := map[string]bool{}
	for _, c := range filterFindings(res.Comments, cfg.Review) {
		if key := repeatKey(c, fingerprint(c)); !seen[key] {
			seen[key] = true
			findings = append(findings, c)
		}
	}
	return &LocalOutcome{Result: res, Findings: findings, GateReason: gateReason(findings, cfg.Review)}, nil
}

// WriteLocal prints a local review's findings for a terminal.
func WriteLocal(w io.Writer, o *LocalOutcome) {
	for _, c := range o.Findings {
		head := plain(c.Path)
		switch start, end := lineSpan(c); {
		case end > start:
			head += fmt.Sprintf(":%d-%d", start, end)
		case start > 0:
			head += fmt.Sprintf(":%d", start)
		}
		if b := strings.Trim(plain(badge(c)), "*"); b != "" {
			head += "  " + b
		}
		fmt.Fprintf(w, "%s\n%s\n", head, indent(strings.TrimSpace(plain(c.Content)), "  "))
		if code := strings.TrimRight(plain(c.SuggestionCode), "\n"); code != "" {
			fmt.Fprintf(w, "\n  Suggested change:\n%s\n", indent(code, "    "))
		}
		fmt.Fprintln(w)
	}
	hidden := len(o.Result.Comments) - len(o.Findings)
	if len(o.Findings) == 0 && hidden == 0 {
		fmt.Fprintln(w, "No findings.")
	} else {
		fmt.Fprintf(w, "%d finding(s)", len(o.Findings))
		if hidden > 0 {
			fmt.Fprintf(w, ", %d more below review.min_severity, outside review.categories, empty or repeated", hidden)
		}
		fmt.Fprintln(w, ".")
	}
	// CI lists these in its summary note.
	for _, wn := range o.Result.Warnings {
		fmt.Fprintf(w, "OCR warning: %s\n", plain(strings.TrimSpace(wn.File+" "+strings.Join(strings.Fields(wn.Message), " "))))
	}
	if !o.Result.Complete() {
		fmt.Fprintf(w, "Warning: OCR's review was incomplete (status %s); CI may report more.\n", plain(o.Result.Status))
	}
	if o.GateReason != "" {
		fmt.Fprintf(w, "CI would fail this merge request: %s\n", o.GateReason)
	}
}

// plain drops control characters other than tab and newline from model output,
// so a finding cannot move the cursor, recolor or retitle the terminal.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
