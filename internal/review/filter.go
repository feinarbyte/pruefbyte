package review

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"pruefbyte/internal/config"
	"pruefbyte/internal/gitlab"
	"pruefbyte/internal/ocr"
)

// filterFindings drops findings below review.min_severity or outside
// review.categories and orders the rest most severe first. Findings with no
// severity or category are kept: there is nothing to filter them on.
func filterFindings(cs []ocr.Comment, r config.Review) []ocr.Comment {
	minRank := config.SeverityRank(r.MinSeverity)
	var cats []string
	for _, c := range r.Categories {
		cats = append(cats, strings.ToLower(strings.TrimSpace(c)))
	}
	var out []ocr.Comment
	for _, c := range cs {
		if strings.TrimSpace(c.Path) == "" || strings.TrimSpace(c.Content) == "" {
			continue
		}
		if rank := config.SeverityRank(c.Severity); rank > 0 && rank < minRank {
			continue
		}
		if cat := strings.ToLower(strings.TrimSpace(c.Category)); len(cats) > 0 && cat != "" && !slices.Contains(cats, cat) {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := config.SeverityRank(out[i].Severity), config.SeverityRank(out[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].StartLine < out[j].StartLine
	})
	return out
}

// skipReason returns why the MR should not be reviewed, or "".
func skipReason(s config.Skip, mr *gitlab.MR) string {
	if s.Drafts && mr.Draft {
		return "merge request is a draft (skip.drafts)"
	}
	for _, l := range mr.Labels {
		if slices.Contains(s.Labels, l) {
			return fmt.Sprintf("label %q (skip.labels)", l)
		}
	}
	if slices.Contains(s.Authors, mr.Author) {
		return fmt.Sprintf("author %q (skip.authors)", mr.Author)
	}
	for _, re := range s.TitleRegex {
		if m, _ := regexp.MatchString(re, mr.Title); m {
			return fmt.Sprintf("title matches %q (skip.title_regex)", re)
		}
	}
	if matchAny(s.SourceBranches, mr.SourceBranch) {
		return fmt.Sprintf("source branch %q (skip.source_branches)", mr.SourceBranch)
	}
	if matchAny(s.TargetBranches, mr.TargetBranch) {
		return fmt.Sprintf("target branch %q (skip.target_branches)", mr.TargetBranch)
	}
	return ""
}

// matchAny matches branch names exactly or as regular expressions anchored at both ends.
func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == s {
			return true
		}
		if re, err := regexp.Compile("^(?:" + p + ")$"); err == nil && re.MatchString(s) {
			return true
		}
	}
	return false
}
