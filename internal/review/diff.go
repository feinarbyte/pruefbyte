package review

import (
	"regexp"
	"strconv"
	"strings"

	"pruefbyte/internal/gitlab"
)

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// lineInfo describes a new-file line that is visible in the MR diff.
type lineInfo struct {
	added   bool
	oldLine int // set for unchanged context lines
}

type fileDiff struct {
	oldPath string
	// lines maps new-file line numbers to diff positions. nil means GitLab
	// returned no patch (too large / collapsed), so lines are unknown.
	lines map[int]lineInfo
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
			fd.lines = parseNewLines(d.Diff)
		}
		idx[d.NewPath] = fd
	}
	return idx
}

func parseNewLines(patch string) map[int]lineInfo {
	lines := map[int]lineInfo{}
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
			lines[newLn] = lineInfo{added: true}
			newLn++
		case strings.HasPrefix(l, "-"):
			oldLn++
		case strings.HasPrefix(l, " "):
			lines[newLn] = lineInfo{oldLine: oldLn}
			oldLn++
			newLn++
		}
	}
	return lines
}

// anchor picks the line a finding on [start,end] should be attached to: the last
// line of the range that is part of the diff. ok is false when no line qualifies.
func (fd *fileDiff) anchor(start, end int) (line int, info lineInfo, ok bool) {
	if end <= 0 {
		end = start
	}
	if start <= 0 || start > end {
		start = end
	}
	if end <= 0 {
		return 0, lineInfo{}, false
	}
	if fd.lines == nil {
		// Unknown patch: try the end line as an added line and let GitLab decide.
		return end, lineInfo{added: true}, true
	}
	for l := end; l >= start; l-- {
		if info, ok := fd.lines[l]; ok {
			return l, info, true
		}
	}
	return 0, lineInfo{}, false
}

// touchedOldRanges returns, per old path, the old-side line ranges of each hunk in
// a diff. Used to tell whether code a previous comment pointed at has changed.
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
		for _, l := range strings.Split(d.Diff, "\n") {
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			end := start + count - 1
			if count == 0 {
				end = start // pure insertion after line `start`
			}
			out[d.OldPath] = append(out[d.OldPath], [2]int{start, end})
		}
	}
	return out
}
