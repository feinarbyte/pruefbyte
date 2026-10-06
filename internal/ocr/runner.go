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
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feinarbyte/pruefbyte/internal/config"
)

type Runner struct {
	Binary string
	// Home is a private directory used as HOME for ocr, so its config.json and
	// session logs never touch (or read from) the runner's real home directory.
	Home string
	// Log receives ocr's stderr as it runs.
	Log io.Writer
	// SecretEnv names env vars ocr must not inherit, such as the renamed
	// gitlab.token_env and llm.api_key_env.
	SecretEnv []string
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
	env := make([]string, 0, len(os.Environ())+9)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		// Env names are case-insensitive on Windows.
		switch k = strings.ToUpper(k); {
		// Keep ocr from reading secrets or config meant for pruefbyte or other tools.
		case k == "HOME" || k == "USERPROFILE" || k == "XDG_CONFIG_HOME":
			continue
		case strings.HasPrefix(k, config.EnvPrefix) || strings.HasPrefix(k, "OCR_LLM_"):
			continue
		case slices.ContainsFunc(r.SecretEnv, func(s string) bool { return strings.EqualFold(s, k) }):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+r.Home, "USERPROFILE="+r.Home,
		// The CI checkout is often owned by another uid; git refuses to work in it otherwise.
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*",
		"TERM=dumb", "NO_COLOR=1",
		// The npm launcher would otherwise check for, and install, a newer OCR on every
		// run: its once-per-18-minutes stamp lives in the fresh HOME.
		"OCR_NO_UPDATE=1",
	)
	// The private HOME also hides ~/.aws, where the AWS SDK behind OCR's Bedrock
	// provider finds profiles and credentials.
	if home, err := os.UserHomeDir(); err == nil {
		for _, f := range [][2]string{{"AWS_CONFIG_FILE", "config"}, {"AWS_SHARED_CREDENTIALS_FILE", "credentials"}} {
			p := filepath.Join(home, ".aws", f[1])
			if _, set := os.LookupEnv(f[0]); !set && fileExists(p) {
				env = append(env, f[0]+"="+p)
			}
		}
	}
	return env
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// command builds an ocr invocation with the isolated environment. On Windows npm
// installs ocr as a .cmd shim, which runs through cmd.exe; Go's argument quoting
// does not protect against cmd.exe, so arguments it would interpret are refused.
// (A '"' only moves cmd.exe's quoting, which matters no more once none of these
// characters is left anywhere on the line.)
func (r *Runner) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	if ext := strings.ToLower(filepath.Ext(cmd.Path)); ext == ".cmd" || ext == ".bat" {
		for _, a := range args {
			if strings.ContainsAny(a, "%&<>^|\r\n") {
				return nil, fmt.Errorf("%s runs through cmd.exe, which cannot be given an argument containing any of %% & < > ^ | or a line break; point ocr.binary at the native OCR executable", cmd.Path)
			}
		}
	}
	cmd.Env = r.env()
	return cmd, nil
}

// ConfigSettings returns the `ocr config set` key/value pairs for the LLM settings.
func ConfigSettings(llm config.LLM, apiKey, language string) ([][2]string, error) {
	section := providerSection(llm.Provider)
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
		b, err := json.Marshal(jsonKeys(llm.ExtraBody))
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
			v := llm.ExtraHeaders[k]
			if strings.Contains(v, ",") {
				v = `"` + v + `"` // ocr splits on commas outside double quotes
			}
			parts = append(parts, k+"="+v)
		}
		add(section+".extra_headers", strings.Join(parts, ","))
	}
	add("language", language)
	return kv, nil
}

// providerSection is the config.json section holding a provider's settings.
func providerSection(provider string) string {
	if config.IsBuiltinProvider(provider) {
		return "providers." + provider
	}
	return "custom_providers." + provider
}

// jsonKeys turns the map[any]any that YAML produces for mappings with non-string
// keys (e.g. logit_bias token ids) into string-keyed maps json can encode.
func jsonKeys(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = jsonKeys(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[fmt.Sprint(k)] = jsonKeys(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = jsonKeys(e)
		}
		return out
	}
	return v
}

// Configure writes the LLM settings into ocr's config via `ocr config set`, so the
// config file format stays ocr's own business.
func (r *Runner) Configure(ctx context.Context, llm config.LLM, apiKey, language string) error {
	settings, err := ConfigSettings(llm, apiKey, language)
	if err != nil {
		return err
	}
	for _, s := range settings {
		// "--": a value starting with '-' (an API key can) is not a flag.
		cmd, err := r.command(ctx, "config", "set", "--", s[0], s[1])
		if err != nil {
			return fmt.Errorf("ocr config set %s: %w", s[0], err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := string(out)
			if apiKey != "" { // an empty old string would put *** between every character
				msg = strings.ReplaceAll(msg, apiKey, "***")
			}
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
	// RuleFile is passed with --rule; exclude patterns go into it too, since
	// --exclude is split on commas and would break brace globs.
	RuleFile       string
	BackgroundFile string
	ExtraArgs      []string
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
	cmd, err := r.command(ctx, o.Args(outFile)...)
	if err != nil {
		return nil, nil, err
	}
	cmd.Dir = o.RepoDir
	// When ctx ends, stop the whole process tree: the npm launcher (node, plus
	// cmd.exe on Windows) runs the native binary as a child, and killing only the
	// launcher leaves it running. On Unix the launcher forwards SIGTERM. WaitDelay
	// bounds the wait for a child that still holds the inherited stderr pipe.
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	var stderr tailBuffer
	cmd.Stdout = r.Log
	cmd.Stderr = io.MultiWriter(r.Log, &stderr)
	runErr := cmd.Run()

	raw, readErr := os.ReadFile(outFile)
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, raw, &RunError{Err: fmt.Errorf("%w (ocr.timeout reached?)", ctx.Err()), Stderr: stderr.String()}
		}
		// A failed run can still leave a usable document (e.g. partial results),
		// but not when OCR itself reports the review as failed.
		if readErr == nil {
			if res, err := ParseResult(raw); err == nil && len(res.Comments) > 0 && !res.Failed() {
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
	cmd, err := r.command(ctx, "version")
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
