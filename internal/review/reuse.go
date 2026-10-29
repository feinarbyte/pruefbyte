package review

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/feinarbyte/pruefbyte/internal/config"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
)

// FullReviewLabel on a merge request turns reuse off for its pipelines.
const FullReviewLabel = "pruefbyte-full"

// reuseVersion is the version of the stored state. A state with another
// version is ignored, which means a full review.
const reuseVersion = 1

// ReuseState records which files a review covered, so a later review of the
// same merge request (or local branch) can skip files whose diff is unchanged.
type ReuseState struct {
	Version int `json:"v"`
	// Identity covers everything besides the files that decides what a review
	// finds: OCR version, model, effort, rules, filters. See reuseIdentity.
	Identity string `json:"id"`
	// Files maps a path (on the head side) to the diff it was reviewed at.
	Files map[string]ReusedFile `json:"files"`
}

type ReusedFile struct {
	// Key is gitutil.Repo.DiffKeys' key: both paths, blobs and modes.
	Key string `json:"k"`
	// At is the head commit of the review that covered the file.
	At string `json:"at"`
}

// ReuseStore keeps the state between local runs.
type ReuseStore interface {
	Load() ([]byte, error)
	Save(data []byte) error
}

// ParseReuseState returns the stored state, or nil when there is none, it
// cannot be read, or it has another version.
func ParseReuseState(data []byte) *ReuseState {
	var s ReuseState
	if len(data) == 0 || json.Unmarshal(data, &s) != nil || s.Version != reuseVersion || s.Identity == "" {
		return nil
	}
	return &s
}

const statePrefix = "<!-- pruefbyte:state="

var statePattern = regexp.MustCompile(`<!-- pruefbyte:state=([A-Za-z0-9_-]+) -->`)

// stateMarker renders s as a hidden comment for the summary note.
func stateMarker(s *ReuseState) string {
	if s == nil {
		return ""
	}
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return statePrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()) + " -->"
}

// stateFromNote reads the state stateMarker wrote into a summary note.
func stateFromNote(body string) *ReuseState {
	ms := statePattern.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(ms[len(ms)-1][1])
	if err != nil {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil
	}
	return ParseReuseState(data)
}

// reuseIdentity hashes what, besides a file's diff, decides what its review
// finds: the OCR version, the settings OCR is configured with (without the
// API key), its review flags, and the rule file before reuse excludes, which
// carries ocr.rules, ocr.rule_file, .opencodereview/rule.json and ocr.exclude.
// OCR's own rule_config_sha256 and runtime_config_sha256 cannot be used: they
// are only known after a run, and the reuse excludes change the file filter
// they cover. The merge request description is left out on purpose.
func reuseIdentity(ocrVersion string, cfg config.Config, ro ocr.ReviewOptions, ruleJSON []byte) string {
	fields := []string{fmt.Sprint(reuseVersion), ocrVersion, string(ruleJSON)}
	settings, _ := ocr.ConfigSettings(cfg.LLM, "", cfg.OCR.Language)
	for _, kv := range settings {
		fields = append(fields, kv[0]+"="+kv[1])
	}
	// Run-specific values (paths, commits) and limits that only decide whether a
	// file is completed, which the manifest reports, are not part of it.
	args := ro.Args("")
	skip := map[string]bool{"--repo": true, "--from": true, "--to": true, "--output": true, "--rule": true,
		"--background-file": true, "--concurrency": true, "--max-tokens-budget": true}
	for i := 0; i < len(args); i++ {
		if skip[args[i]] {
			i++
			continue
		}
		fields = append(fields, args[i])
	}
	h := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(h[:])
}

// reusePlan is what a review takes over from the stored state.
type reusePlan struct {
	identity string
	keys     map[string]string // DiffKeys of this review's diff
	// reused are the files OCR is not given this time, with the review that covered them.
	reused map[string]ReusedFile
}

// excludable reports whether path can be passed to OCR as an exclude pattern
// that matches it alone. OCR matches excludes as globs, expands braces without
// honoring escapes, and compares case-insensitively.
func excludable(path string) bool {
	return path != "" && !strings.ContainsAny(path, `*?[]{}\`)
}

// planReuse picks the changed files whose diff is the one prev covered.
func planReuse(prev *ReuseState, identity string, keys map[string]string) *reusePlan {
	p := &reusePlan{identity: identity, keys: keys, reused: map[string]ReusedFile{}}
	if prev == nil || prev.Identity != identity {
		return p
	}
	for path, k := range keys {
		if f, ok := prev.Files[path]; ok && f.Key == k && excludable(path) {
			p.reused[path] = f
		}
	}
	// An exclude would also hit a changed file that differs only in case.
	for path := range keys {
		if _, ok := p.reused[path]; ok {
			continue
		}
		for r := range p.reused {
			if strings.EqualFold(r, path) {
				delete(p.reused, r)
			}
		}
	}
	return p
}

// excludes returns the reused files as OCR exclude patterns.
func (p *reusePlan) excludes() []string {
	out := make([]string, 0, len(p.reused))
	for path := range p.reused {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (p *reusePlan) excluded(path string) bool {
	if p == nil {
		return false
	}
	_, ok := p.reused[path]
	return ok
}

// next builds the state to store after a review at head: the reused files, plus
// the files OCR completed this time without a single comment or warning. A file
// OCR commented on is reviewed again next time: its findings, the gate they may
// trip and the summary lines they may need are only ever produced by a review,
// and once fixed the file's diff changes anyway.
func (p *reusePlan) next(res *ocr.Result, head string) *ReuseState {
	s := &ReuseState{Version: reuseVersion, Identity: p.identity, Files: map[string]ReusedFile{}}
	for path, f := range p.reused {
		s.Files[path] = f
	}
	if res == nil || res.Manifest == nil {
		return s
	}
	flagged := map[string]bool{}
	for _, c := range res.Comments {
		flagged[c.Path] = true
	}
	for _, w := range res.Warnings {
		flagged[w.File] = true
	}
	for _, it := range res.Manifest.Coverage.Completed {
		if k := p.keys[it.Path]; k != "" && !flagged[it.Path] && excludable(it.Path) && !p.excluded(it.Path) {
			s.Files[it.Path] = ReusedFile{Key: k, At: head}
		}
	}
	return s
}

// nothingLeft reports whether every file OCR would have reviewed was reused.
func (p *reusePlan) nothingLeft(res *ocr.Result) bool {
	return p != nil && len(p.reused) > 0 && res != nil && res.Complete() &&
		(res.Manifest == nil || len(res.Manifest.Coverage.Selected) == 0)
}

// reusedHeads lists the commits the reused files were reviewed at, shortened.
func (p *reusePlan) reusedHeads() []string {
	var heads []string
	for _, f := range p.reused {
		h := f.At
		if len(h) > 8 {
			h = h[:8]
		}
		if !slices.Contains(heads, h) {
			heads = append(heads, h)
		}
	}
	sort.Strings(heads)
	return heads
}

// preparePlan computes the reuse plan for a review of base..head, or nil when
// the changed files cannot be keyed (then everything is reviewed and nothing
// is stored).
func preparePlan(ctx context.Context, d Deps, identity string, prev *ReuseState, base, head string, logf func(string, ...any)) *reusePlan {
	if d.DiffKeys == nil {
		return nil
	}
	keys, err := d.DiffKeys(ctx, base, head)
	if err != nil {
		logf("warning: reviewing every file: %v", err)
		return nil
	}
	return planReuse(prev, identity, keys)
}
