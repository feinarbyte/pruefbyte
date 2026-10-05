package review

import (
	"regexp"
	"strconv"
	"strings"

	"pruefbyte/internal/gitlab"
	"pruefbyte/internal/ocr"
)

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// lineInfo describes a new-file line that is visible in the MR diff.
type lineInfo struct {
	added   bool
	oldLine int // set for unchanged context lines
	text    string
}

// oldLineInfo describes an old-file line that is visible in the MR diff.
type oldLineInfo struct {
	removed bool
	newLine int // set for unchanged context lines
	text    string
}

type fileDiff struct {
	oldPath string
	// lines and oldLines map new- and old-file line numbers to diff positions. nil
	// means GitLab returned no patch (too large / collapsed), so lines are unknown.
	lines    map[int]lineInfo
	oldLines map[int]oldLineInfo
}

// diffIndex maps new paths of the MR diff to the lines GitLab can anchor comments on.
type diffIndex map[string]*fileDiff

func buildDiffIndex(diffs []gitlab.FileDiff) diffIndex {
	idx := diffIndex{}
	for _, d := range diffs {
		if d.DeletedFile {
			continue
		}
		fd := &fileDiff{oldPath: d.OldPath}
		if strings.TrimSpace(d.Diff) != "" {
			fd.lines, fd.oldLines = parsePatch(d.Diff)
		}
		idx[d.NewPath] = fd
	}
	return idx
}

func parsePatch(patch string) (map[int]lineInfo, map[int]oldLineInfo) {
	lines, oldLines := map[int]lineInfo{}, map[int]oldLineInfo{}
	oldLn, newLn, inHunk := 0, 0, false
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			oldLn, _ = strconv.Atoi(m[1])
			newLn, _ = strconv.Atoi(m[3])
			inHunk = true
			continue
		}
		if !inHunk || strings.HasPrefix(l, `\`) {
			continue
		}
		switch {
		case strings.HasPrefix(l, "+"):
			lines[newLn] = lineInfo{added: true, text: l[1:]}
			newLn++
		case strings.HasPrefix(l, "-"):
			oldLines[oldLn] = oldLineInfo{removed: true, text: l[1:]}
			oldLn++
		case strings.HasPrefix(l, " "):
			lines[newLn] = lineInfo{oldLine: oldLn, text: l[1:]}
			oldLines[oldLn] = oldLineInfo{newLine: newLn, text: l[1:]}
			oldLn++
			newLn++
		}
	}
	return lines, oldLines
}

// placement is where a finding goes on the diff.
type placement struct {
	newLine, oldLine int // GitLab position lines; newLine 0 means a removed line
	at               int // the same line in the finding's own numbering
	// suggest is set when the new-file lines start..end are in the diff and hold
	// the finding's existing_code, so a suggestion block replaces exactly that code.
	suggest bool
}

func (p placement) line() int {
	if p.newLine > 0 {
		return p.newLine
	}
	return p.oldLine
}

// place picks the diff line a finding is attached to. OCR numbers lines of the
// new file, except when existing_code only occurs among context and removed
// lines: then they are old-file numbers, and the comment goes on the old side.
func (fd *fileDiff) place(c ocr.Comment) (placement, bool) {
	start, end := c.StartLine, c.EndLine
	if end <= 0 {
		end = start
	}
	if start <= 0 || start > end {
		start = end
	}
	if end <= 0 {
		return placement{}, false
	}
	if fd.lines == nil {
		// Unknown patch: try the end line as an added line and let GitLab decide.
		return placement{newLine: end, at: end}, true
	}
	if code := codeLines(c.ExistingCode); code != nil {
		if matchLines(code, start, end, func(l int) (string, bool) { i, ok := fd.lines[l]; return i.text, ok }) {
			return placement{newLine: end, oldLine: fd.lines[end].oldLine, at: end, suggest: true}, true
		}
		if matchLines(code, start, end, func(l int) (string, bool) { i, ok := fd.oldLines[l]; return i.text, ok }) {
			return placement{newLine: fd.oldLines[end].newLine, oldLine: end, at: end}, true
		}
	}
	for l := end; l >= start; l-- {
		if info, ok := fd.lines[l]; ok {
			return placement{newLine: l, oldLine: info.oldLine, at: l}, true
		}
	}
	return placement{}, false
}

// codeLines splits code into whitespace-normalized lines, or nil if it is blank.
func codeLines(code string) []string {
	code = strings.Trim(code, "\n")
	if strings.TrimSpace(code) == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(code, "\n") {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return out
}

// matchLines reports whether lines start..end all exist and equal code.
func matchLines(code []string, start, end int, get func(int) (string, bool)) bool {
	if end-start+1 != len(code) {
		return false
	}
	for i, want := range code {
		got, ok := get(start + i)
		if !ok || strings.Join(strings.Fields(got), " ") != want {
			return false
		}
	}
	return true
}

// touchedOldRanges returns, per old path, the old-side lines a diff removes or
// replaces, plus the line after which a pure insertion happens. Used to tell
// whether code a previous comment pointed at has changed. Context lines do not
// count: GitLab puts a few unchanged lines around every hunk.
func touchedOldRanges(diffs []gitlab.FileDiff) map[string][][2]int {
	out := map[string][][2]int{}
	for _, d := range diffs {
		if d.NewFile {
			continue
		}
		if d.DeletedFile || d.RenamedFile || strings.TrimSpace(d.Diff) == "" {
			out[d.OldPath] = append(out[d.OldPath], [2]int{1, int(^uint(0) >> 1)})
			continue
		}
		touch := func(line int) {
			if line < 1 {
				return
			}
			rs := out[d.OldPath]
			if n := len(rs); n > 0 && line >= rs[n-1][0] && line <= rs[n-1][1]+1 {
				rs[n-1][1] = max(rs[n-1][1], line)
				return
			}
			out[d.OldPath] = append(rs, [2]int{line, line})
		}
		oldLn, inHunk, removed := 0, false, false
		for _, l := range strings.Split(d.Diff, "\n") {
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				oldLn, _ = strconv.Atoi(m[1])
				if m[2] == "0" {
					oldLn++ // an empty old side names the line it follows
				}
				inHunk, removed = true, false
				continue
			}
			if !inHunk || strings.HasPrefix(l, `\`) {
				continue
			}
			switch {
			case strings.HasPrefix(l, "-"):
				touch(oldLn)
				oldLn++
				removed = true
			case strings.HasPrefix(l, "+"):
				if !removed {
					touch(oldLn - 1) // pure insertion after this old line
				}
			case strings.HasPrefix(l, " "):
				oldLn++
				removed = false
			}
		}
	}
	return out
}
