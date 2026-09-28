// Package ocr drives the OpenCodeReview CLI (`ocr`) in an isolated home directory.
package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"pruefbyte/internal/config"
)

type Runner struct {
	Binary string
	// Home is a private directory used as HOME for ocr, so its config.json and
	// session logs never touch (or read from) the runner's real home directory.
	Home string
	// Log receives ocr's stderr as it runs.
	Log io.Writer
}

// NewRunner creates a runner with a fresh temporary home directory. Call Close to remove it.
func NewRunner(binary string, log io.Writer) (*Runner, error) {
	home, err := os.MkdirTemp("", "pruefbyte-ocr-")
	if err != nil {
		return nil, err
	}
	return &Runner{Binary: binary, Home: home, Log: log}, nil
}

func (r *Runner) Close() error { return os.RemoveAll(r.Home) }

func (r *Runner) env() []string {
	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		// Keep ocr from reading secrets or config meant for pruefbyte or other tools.
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME":
			continue
		}
		if strings.HasPrefix(k, "PRUEFBYTE_") || strings.HasPrefix(k, "OCR_LLM_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+r.Home, "USERPROFILE="+r.Home,
		// The CI checkout is often owned by another uid; git refuses to work in it otherwise.
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*",
		"TERM=dumb", "NO_COLOR=1",
	)
}

// ConfigSettings returns the `ocr config set` key/value pairs for the LLM settings.
func ConfigSettings(llm config.LLM, apiKey, language string) ([][2]string, error) {
	section := "providers." + llm.Provider
	if !config.IsBuiltinProvider(llm.Provider) {
		section = "custom_providers." + llm.Provider
	}
	var kv [][2]string
	add := func(k, v string) {
		if v != "" {
			kv = append(kv, [2]string{k, v})
		}
	}
	add("provider", llm.Provider)
	add("model", llm.Model)
	if strings.HasPrefix(section, "custom_") {
		add(section+".protocol", llm.Protocol)
		add(section+".model", llm.Model)
		if apiKey == "" && llm.Protocol != "anthropic-bedrock" {
			// OCR requires a non-empty key for custom providers; keyless local endpoints accept anything.
			apiKey = "none"
		}
	}
	add(section+".url", llm.URL)
	add(section+".api_key", apiKey)
	if llm.TimeoutSec > 0 {
		add(section+".timeout_sec", strconv.Itoa(llm.TimeoutSec))
	}
	add(section+".aws_region", llm.AWSRegion)
	add(section+".aws_profile", llm.AWSProfile)
	if len(llm.ExtraBody) > 0 {
		b, err := json.Marshal(llm.ExtraBody)
		if err != nil {
			return nil, fmt.Errorf("llm.extra_body: %w", err)
		}
		add(section+".extra_body", string(b))
	}
	if len(llm.ExtraHeaders) > 0 {
		keys := make([]string, 0, len(llm.ExtraHeaders))
		for k := range llm.ExtraHeaders {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+llm.ExtraHeaders[k])
		}
		add(section+".extra_headers", strings.Join(parts, ","))
	}
	add("language", language)
	return kv, nil
}

// Configure writes the LLM settings into ocr's config via `ocr config set`, so the
// config file format stays ocr's own business.
func (r *Runner) Configure(ctx context.Context, llm config.LLM, apiKey, language string) error {
	settings, err := ConfigSettings(llm, apiKey, language)
	if err != nil {
		return err
	}
	for _, s := range settings {
		cmd := exec.CommandContext(ctx, r.Binary, "config", "set", s[0], s[1])
		cmd.Env = r.env()
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.ReplaceAll(string(out), apiKey, "***")
			return fmt.Errorf("ocr config set %s: %w: %s", s[0], err, strings.TrimSpace(msg))
		}
	}
	return nil
}

type ReviewOptions struct {
	RepoDir         string
	From, To        string
	Provider, Model string
	Effort          string
	Concurrency     int
	MaxTokensBudget int
	Exclude         []string
	RuleFile        string
	BackgroundFile  string
	ExtraArgs       []string
}

func (o ReviewOptions) Args(outFile string) []string {
	args := []string{"review", "--repo", o.RepoDir, "--from", o.From, "--to", o.To,
		"--format", "json", "--audience", "agent", "--output", outFile}
	if o.Provider != "" {
		args = append(args, "--provider", o.Provider)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(o.Concurrency))
	}
	if o.MaxTokensBudget > 0 {
		args = append(args, "--max-tokens-budget", strconv.Itoa(o.MaxTokensBudget))
	}
	if len(o.Exclude) > 0 {
		args = append(args, "--exclude", strings.Join(o.Exclude, ","))
	}
	if o.RuleFile != "" {
		args = append(args, "--rule", o.RuleFile)
	}
	if o.BackgroundFile != "" {
		args = append(args, "--background-file", o.BackgroundFile)
	}
	return append(args, o.ExtraArgs...)
}

// RunError carries ocr's stderr tail when the review process fails.
type RunError struct {
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	return fmt.Sprintf("ocr review failed: %v\n%s", e.Err, e.Stderr)
}

func (e *RunError) Unwrap() error { return e.Err }

// Review runs `ocr review` and returns the parsed JSON result. The raw output is
// also returned so callers can keep it as a CI artifact.
func (r *Runner) Review(ctx context.Context, o ReviewOptions) (*Result, []byte, error) {
	outFile := filepath.Join(r.Home, "result.json")
	cmd := exec.CommandContext(ctx, r.Binary, o.Args(outFile)...)
	cmd.Dir = o.RepoDir
	cmd.Env = r.env()
	var stderr tailBuffer
	cmd.Stdout = r.Log
	cmd.Stderr = io.MultiWriter(r.Log, &stderr)
	runErr := cmd.Run()

	raw, readErr := os.ReadFile(outFile)
	if runErr != nil {
		if ctx.Err() != nil {
			runErr = fmt.Errorf("%w (ocr.timeout reached?)", ctx.Err())
		}
		// A failed run can still leave a usable document (e.g. partial results).
		if readErr == nil {
			if res, err := ParseResult(raw); err == nil && len(res.Comments) > 0 {
				res.Status = "partial"
				return res, raw, nil
			}
		}
		return nil, raw, &RunError{Err: runErr, Stderr: stderr.String()}
	}
	if readErr != nil {
		return nil, nil, &RunError{Err: fmt.Errorf("ocr wrote no result file: %w", readErr), Stderr: stderr.String()}
	}
	res, err := ParseResult(raw)
	if err != nil {
		return nil, raw, &RunError{Err: err, Stderr: stderr.String()}
	}
	return res, raw, nil
}

// WriteTemp writes data to a file in the runner's home and returns its path.
func (r *Runner) WriteTemp(name string, data []byte) (string, error) {
	p := filepath.Join(r.Home, name)
	return p, os.WriteFile(p, data, 0o600)
}

// tailBuffer keeps the last 8 KiB written to it.
type tailBuffer struct{ buf bytes.Buffer }

const tailSize = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - tailSize; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(t.buf.String()) }

var ErrNotFound = errors.New("ocr binary not found")

// Version returns `ocr version` output, or ErrNotFound.
func (r *Runner) Version(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(r.Binary); err != nil {
		return "", fmt.Errorf("%w: %q (install @alibaba-group/open-code-review or set ocr.binary)", ErrNotFound, r.Binary)
	}
	cmd := exec.CommandContext(ctx, r.Binary, "version")
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
