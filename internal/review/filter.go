package review

import (
	"slices"
	"sort"
	"strings"

	"pruefbyte/internal/config"
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
