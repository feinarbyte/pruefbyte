package ocr

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/feinarbyte/pruefbyte/internal/config"
)

// RuleFile is OCR's rule.json schema.
type RuleFile struct {
	Include []string      `json:"include,omitempty"`
	Exclude []string      `json:"exclude,omitempty"`
	Rules   []config.Rule `json:"rules,omitempty"`
}

// ParseRuleFile reads an OCR rule.json document.
func ParseRuleFile(data []byte) (RuleFile, error) {
	var rf RuleFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return rf, fmt.Errorf("parsing rule.json: %w", err)
	}
	return rf, nil
}

var ruleFileExts = map[string]bool{".md": true, ".txt": true, ".markdown": true}

// InlineFiles replaces rules that name a file instead of holding text (OCR's
// test: one line, no spaces, ending in .md, .txt or .markdown) with the content
// read returns for that name. A file read cannot find leaves the rule empty, as
// in OCR.
func (rf *RuleFile) InlineFiles(read func(name string) ([]byte, bool, error)) error {
	for i := range rf.Rules {
		name := rf.Rules[i].Rule
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\n ") || !ruleFileExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		data, found, err := read(name)
		if err != nil {
			return fmt.Errorf("rule file %s: %w", name, err)
		}
		rf.Rules[i].Rule = ""
		if found {
			rf.Rules[i].Rule = strings.TrimRight(string(data), "\n")
		}
	}
	return nil
}

// RuleJSON merges rule layers, highest priority first, and extra exclude patterns
// into the one file pruefbyte passes with --rule.
//
// OCR also reads <repo>/.opencodereview/rule.json from the checkout, which the
// merge request controls. The result keeps that file inert: OCR takes the file
// filter from the highest-priority layer that has include/exclude patterns, so
// this file always has some, and the trailing catch-all matches every path, so
// the checkout's rule entries are never reached. The catch-all has no text and
// keeps OCR's built-in rule, so it changes nothing else.
func RuleJSON(layers []RuleFile, exclude []string) ([]byte, error) {
	var out RuleFile
	for _, l := range layers {
		out.Rules = append(out.Rules, l.Rules...)
		if len(out.Include)+len(out.Exclude) == 0 {
			out.Include, out.Exclude = slices.Clone(l.Include), slices.Clone(l.Exclude)
		}
	}
	out.Exclude = append(out.Exclude, exclude...)
	if len(out.Include)+len(out.Exclude) == 0 {
		out.Exclude = []string{".git/**"} // matches nothing a diff contains
	}
	out.Rules = append(out.Rules, config.Rule{Path: "**/*", MergeSystemRule: true})
	return json.MarshalIndent(out, "", "  ")
}
