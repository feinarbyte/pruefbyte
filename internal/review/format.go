package review

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"pruefbyte/internal/ocr"
)

const (
	fpPrefix      = "<!-- pruefbyte:fp="
	summaryMarker = "<!-- pruefbyte:summary -->"
)

var fpPattern = regexp.MustCompile(`<!-- pruefbyte:fp=([0-9a-f]+) -->`)

// fingerprint identifies a finding independently of its line numbers, so the same
// issue is recognised after unrelated lines shift.
func fingerprint(c ocr.Comment) string {
	norm := strings.Join(strings.Fields(strings.ToLower(c.Content)), " ")
	h := sha1.Sum([]byte(c.Path + "\x00" + norm))
	return hex.EncodeToString(h[:8])
}

func extractFingerprint(body string) string {
	if m := fpPattern.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

var severityIcon = map[string]string{"critical": "🔴", "high": "🟠", "medium": "🟡", "low": "🔵"}

func badge(c ocr.Comment) string {
	sev := strings.ToLower(strings.TrimSpace(c.Severity))
	cat := strings.ToLower(strings.TrimSpace(c.Category))
	var parts []string
	if sev != "" {
		icon := severityIcon[sev]
		if icon == "" {
			icon = "⚪"
		}
		parts = append(parts, icon+" "+strings.ToUpper(sev[:1])+sev[1:])
	}
	if cat != "" {
		parts = append(parts, cat)
	}
	if len(parts) == 0 {
		return ""
	}
	return "**" + strings.Join(parts, " · ") + "**"
}

func lineLabel(c ocr.Comment) string {
	switch {
	case c.StartLine > 0 && c.EndLine > c.StartLine:
		return fmt.Sprintf("lines %d–%d", c.StartLine, c.EndLine)
	case c.EndLine > 0:
		return fmt.Sprintf("line %d", c.EndLine)
	case c.StartLine > 0:
		return fmt.Sprintf("line %d", c.StartLine)
	}
	return ""
}

// fence returns a backtick fence longer than any backtick run inside code.
func fence(code string) string {
	f := "```"
	for strings.Contains(code, f) {
		f += "`"
	}
	return f
}

// inlineBody renders a finding as a diff discussion anchored at anchorLine.
func inlineBody(c ocr.Comment, fp string, anchorLine int, suggestions bool) string {
	var b strings.Builder
	head := badge(c)
	if anchorLine != c.EndLine || c.EndLine > c.StartLine && c.StartLine > 0 {
		if l := lineLabel(c); l != "" {
			if head != "" {
				head += " "
			}
			head += "(" + l + ")"
		}
	}
	if head != "" {
		b.WriteString(head + "\n\n")
	}
	b.WriteString(strings.TrimSpace(c.Content))
	b.WriteString("\n")

	if code := strings.TrimRight(c.SuggestionCode, "\n"); code != "" && suggestions {
		f := fence(code)
		// A GitLab suggestion replaces the anchor line plus N lines above it, which
		// only matches OCR's range when we anchored on its last line.
		if c.ExistingCode != "" && anchorLine == c.EndLine && c.StartLine > 0 && c.StartLine <= c.EndLine {
			fmt.Fprintf(&b, "\n%ssuggestion:-%d+0\n%s\n%s\n", f, c.EndLine-c.StartLine, code, f)
		} else {
			fmt.Fprintf(&b, "\nSuggested change:\n\n%s\n%s\n%s\n", f, code, f)
		}
	}
	fmt.Fprintf(&b, "\n%s%s -->", fpPrefix, fp)
	return b.String()
}

type summaryData struct {
	Model       string
	HeadSHA     string
	Stats       Stats
	Unplaced    []ocr.Comment
	Overflow    []ocr.Comment
	MaxComments int
	Warnings    []ocr.Warning
	Failure     string
	Incomplete  bool
}

func summaryBody(d summaryData) string {
	var b strings.Builder
	b.WriteString(summaryMarker + "\n### 🔍 pruefbyte review\n\n")
	sha := d.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	if d.Failure != "" {
		fmt.Fprintf(&b, "⚠️ The review of `%s` failed.\n\n<details><summary>Error</summary>\n\n```\n%s\n```\n\n</details>\n", sha, d.Failure)
		return b.String()
	}
	fmt.Fprintf(&b, "Reviewed `%s`", sha)
	if d.Model != "" {
		fmt.Fprintf(&b, " with `%s`", d.Model)
	}
	fmt.Fprintf(&b, ": %d new inline comment(s), %d already posted earlier.\n", d.Stats.Posted, d.Stats.Duplicates)
	if d.Incomplete {
		b.WriteString("\n⚠️ The review did not complete for every file; see the job log.\n")
	}
	list := func(title string, cs []ocr.Comment) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n#### %s\n\n", title)
		for _, c := range cs {
			loc := c.Path
			if l := lineLabel(c); l != "" {
				loc += " (" + l + ")"
			}
			line := "- `" + loc + "`"
			if bd := badge(c); bd != "" {
				line += " " + bd
			}
			content := strings.Join(strings.Fields(c.Content), " ")
			if r := []rune(content); len(r) > 400 {
				content = string(r[:400]) + "…"
			}
			b.WriteString(line + ": " + content + "\n")
		}
	}
	list("Findings outside the diff", d.Unplaced)
	list(fmt.Sprintf("Further findings (over the limit of %d inline comments)", d.MaxComments), d.Overflow)
	if len(d.Warnings) > 0 {
		b.WriteString("\n<details><summary>OCR warnings</summary>\n\n")
		for _, w := range d.Warnings {
			fmt.Fprintf(&b, "- %s %s\n", w.File, strings.Join(strings.Fields(w.Message), " "))
		}
		b.WriteString("\n</details>\n")
	}
	if len(d.Unplaced)+len(d.Overflow)+len(d.Warnings) == 0 && !d.Incomplete {
		b.WriteString("\nAll findings are posted inline.\n")
	}
	return b.String()
}
