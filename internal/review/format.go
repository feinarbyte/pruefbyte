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
// issue is recognised after unrelated lines shift. The model words a finding
// differently from run to run, so the code it points at (existing_code, which OCR's
// comment tool asks for) identifies it, as in OCR's own session comparison; the
// text is only a fallback.
func fingerprint(c ocr.Comment) string {
	key := "text\x00" + strings.ToLower(strings.Join(strings.Fields(c.Content), " "))
	if code := codeLines(c.ExistingCode); code != nil {
		key = "code\x00" + strings.ToLower(strings.TrimSpace(c.Category)) + "\x00" + strings.Join(code, "\n")
	}
	h := sha1.Sum([]byte(c.Path + "\x00" + key))
	return hex.EncodeToString(h[:8])
}

// extractFingerprint returns the marker inlineBody appends last; the finding text
// before it comes from the model and may contain marker-like text.
func extractFingerprint(body string) string {
	if ms := fpPattern.FindAllStringSubmatch(body, -1); len(ms) > 0 {
		return ms[len(ms)-1][1]
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

// inlineBody renders a finding as a diff discussion placed at pl.
func inlineBody(c ocr.Comment, fp string, pl placement, suggestions bool) string {
	var b strings.Builder
	head := badge(c)
	if pl.at != c.EndLine || c.EndLine > c.StartLine && c.StartLine > 0 {
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
	b.WriteString(neutralizeQuickActions(strings.TrimSpace(c.Content)))
	b.WriteString("\n")

	if code := strings.TrimRight(c.SuggestionCode, "\n"); code != "" && suggestions {
		f := fence(code)
		// A GitLab suggestion replaces the anchor line plus N lines above it. That is
		// only safe when those lines hold the code OCR quoted (see place).
		if pl.suggest && c.StartLine > 0 && c.StartLine <= c.EndLine {
			fmt.Fprintf(&b, "\n%ssuggestion:-%d+0\n%s\n%s\n", f, c.EndLine-c.StartLine, code, f)
		} else {
			fmt.Fprintf(&b, "\nSuggested change:\n\n%s\n%s\n%s\n", f, code, f)
		}
	}
	fmt.Fprintf(&b, "\n%s%s -->", fpPrefix, fp)
	return b.String()
}

// neutralizeQuickActions escapes a leading "/" on lines outside code fences.
// GitLab runs such lines in the bot's comments as quick actions (/approve,
// /merge, /close, ...), and the text comes from a model that reads the MR.
// "\/" renders as "/".
func neutralizeQuickActions(s string) string {
	lines := strings.Split(s, "\n")
	fence := ""
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		switch {
		case fence != "":
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
		case strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"):
			fence = t[:3]
		case strings.HasPrefix(t, "/"):
			lines[i] = l[:len(l)-len(t)] + `\` + t
		}
	}
	return strings.Join(lines, "\n")
}

type summaryData struct {
	Model       string
	HeadSHA     string
	Stats       Stats
	Unplaced    []ocr.Comment
	Overflow    []ocr.Comment
	Failed      []ocr.Comment // could not be posted
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
		f := fence(d.Failure)
		fmt.Fprintf(&b, "⚠️ The review of `%s` failed.\n\n<details><summary>Error</summary>\n\n%s\n%s\n%s\n\n</details>\n", sha, f, d.Failure, f)
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
	list("Findings that could not be posted (see the job log)", d.Failed)
	if len(d.Warnings) > 0 {
		b.WriteString("\n<details><summary>OCR warnings</summary>\n\n")
		for _, w := range d.Warnings {
			fmt.Fprintf(&b, "- %s %s\n", w.File, strings.Join(strings.Fields(w.Message), " "))
		}
		b.WriteString("\n</details>\n")
	}
	if len(d.Unplaced)+len(d.Overflow)+len(d.Failed)+len(d.Warnings) == 0 && !d.Incomplete {
		b.WriteString("\nAll findings are posted inline.\n")
	}
	return b.String()
}
