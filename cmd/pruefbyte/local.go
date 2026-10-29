package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/feinarbyte/pruefbyte/internal/config"
	"github.com/feinarbyte/pruefbyte/internal/gitutil"
	"github.com/feinarbyte/pruefbyte/internal/ocr"
	"github.com/feinarbyte/pruefbyte/internal/review"
)

type localFlags struct {
	repoDir   string
	target    string
	committed bool
	format    string
	full      bool
}

func localCmd(g *globalFlags) *cobra.Command {
	f := &localFlags{}
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Review your branch locally with the same settings as CI",
		Long: `Review the current branch the way the CI job will review its merge request,
and print the findings instead of posting them.

The review covers everything from the merge base with the target branch up to
your working tree: commits, staged and unstaged changes and untracked files.
--committed limits it to commits, which is exactly what CI sees after a push.
Settings come from the same places as in CI: the global config (--config),
.pruefbyte.yml and OCR rule files at the target branch, and PRUEFBYTE_* env vars.

The API key comes from the env var named by llm.api_key_env
(PRUEFBYTE_LLM_API_KEY), else from your own OCR setup
(~/.opencodereview/config.json), else from the provider's env var such as
ANTHROPIC_API_KEY. No GitLab token is needed.

Files whose diff is unchanged since the last run on the same branch and
target, and on which that run found nothing, are not reviewed again
(review.reuse_unchanged); --full reviews every file.

Exits 3 if a finding reaches review.fail_on_severity, as in CI.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocal(cmd.Context(), cmd.OutOrStdout(), g, f)
		},
	}
	cmd.Flags().StringVar(&f.repoDir, "repo", "", "repository to review (default: the one containing the current directory)")
	cmd.Flags().StringVar(&f.target, "target", "", "branch the merge request goes into (default: origin's HEAD, e.g. origin/main)")
	cmd.Flags().BoolVar(&f.committed, "committed", false, "review only committed changes, ignoring the working tree")
	cmd.Flags().StringVar(&f.format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&f.full, "full", false, "review every file, also those unchanged since the last run")
	return cmd
}

func runLocal(ctx context.Context, stdout io.Writer, g *globalFlags, f *localFlags) error {
	if f.format != "text" && f.format != "json" {
		return fmt.Errorf("--format must be text or json, got %q", f.format)
	}
	base, err := baseConfig(g)
	if err != nil {
		return err
	}
	dir := f.repoDir
	if dir == "" {
		dir = "."
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	repo := gitutil.Repo{Dir: dir}
	top, err := repo.Toplevel(ctx)
	if err != nil {
		return fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	repo.Dir = top

	target := f.target
	if target == "" {
		if target, err = repo.DefaultTarget(ctx); err != nil {
			return err
		}
	}
	head, err := repo.Head(ctx)
	if err != nil {
		return err
	}
	mergeBase, err := repo.MergeBase(ctx, target, head)
	if err != nil {
		return err
	}
	to := head
	if !f.committed {
		if to, err = repo.Snapshot(ctx, head); err != nil {
			return fmt.Errorf("snapshotting the working tree: %w", err)
		}
	}
	if mergeBase == to {
		fmt.Fprintf(os.Stderr, "[pruefbyte] nothing to review: no changes against %s\n", target)
		return nil
	}

	// In CI the merge request's title and description are OCR background; locally
	// the branch name and commit messages stand in for them.
	branch, _ := repo.Branch(ctx)
	log, _ := repo.Log(ctx, mergeBase, head)

	apiKey := os.Getenv(base.LLM.APIKeyEnv)
	runner, err := newRunner(base.OCR.Binary)
	if err != nil {
		return err
	}
	defer runner.Close()
	runner.SecretEnv = []string{base.GitLab.TokenEnv, base.LLM.APIKeyEnv}
	v, err := runner.Version(ctx)
	if err != nil {
		return err
	}
	scope := "commits and working tree"
	if f.committed {
		scope = "commits only"
	}
	fmt.Fprintf(os.Stderr, "[pruefbyte] %s, %s\n", version, strings.SplitN(v, "\n", 2)[0])
	fmt.Fprintf(os.Stderr, "[pruefbyte] reviewing %s against %s (merge base %.8s, %s); this can take a few minutes\n",
		branchOr(branch, head), target, mergeBase, scope)

	deps := review.Deps{
		OCR:        runner,
		Log:        os.Stderr,
		LoadConfig: configLoader(g, repo, true),
		PrepareOCR: func(ctx context.Context, cfg config.Config) error {
			key := apiKey
			if key == "" {
				uc := ocr.UserCredential(cfg.LLM.Provider)
				// A key the user's setup sends to its own endpoint (a gateway, a proxy)
				// must not go to the endpoint this review uses instead.
				if (uc.APIKey != "" || uc.APIKeyCmd != "") && uc.URL != "" &&
					strings.TrimRight(uc.URL, "/") != strings.TrimRight(cfg.LLM.URL, "/") {
					endpoint := cfg.LLM.URL
					if endpoint == "" {
						endpoint = "the provider's default endpoint"
					}
					fmt.Fprintf(os.Stderr, "[pruefbyte] not using the %s API key from your OCR config: it is for %s, but this review uses %s\n",
						cfg.LLM.Provider, uc.URL, endpoint)
					uc = ocr.UserCred{}
				}
				key = uc.APIKey
				if cmd := uc.APIKeyCmd; key == "" && cmd != "" {
					// ocr would run the command under pruefbyte's private HOME, where
					// keychains, pass or ~/ files are not found; run it here instead.
					var err error
					if key, err = ocr.RunKeyCommand(ctx, cmd); err != nil {
						return fmt.Errorf("api_key_cmd for %s from your OCR config: %w", cfg.LLM.Provider, err)
					}
				}
				if key != "" {
					fmt.Fprintf(os.Stderr, "[pruefbyte] using the %s API key from your OCR config\n", cfg.LLM.Provider)
				}
			}
			return runner.Configure(ctx, cfg.LLM, key, cfg.OCR.Language)
		},
		ReadFileAt: repo.ShowFile,
		DiffKeys:   repo.DiffKeys,
		OCRVersion: v,
	}
	// What earlier runs covered is kept per branch and target, in the git
	// directory all worktrees share. A detached HEAD has no branch to keep it for.
	if branch != "" {
		if common, err := repo.CommonDir(ctx); err == nil {
			h := sha256.Sum256([]byte(branch + "\x00" + target))
			deps.ReuseStore = fileStore(filepath.Join(common, "pruefbyte", "reuse-"+hex.EncodeToString(h[:8])+".json"))
		}
	}
	out, err := review.Local(ctx, deps, review.LocalTarget{
		BaseSHA: mergeBase, HeadSHA: to, Title: branch, Description: log, Full: f.full,
	}, repo.Dir)
	if err != nil {
		return err
	}

	if f.format == "json" {
		if out.Findings == nil {
			out.Findings = []ocr.Comment{} // "findings": [], not null
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(localJSON{
			Status: out.Result.Status, Model: out.Result.LLM.Model, Base: mergeBase, Head: to,
			Findings: out.Findings, Gate: out.GateReason, Reused: out.Reused,
		}); err != nil {
			return err
		}
	} else {
		review.WriteLocal(stdout, out)
	}
	if out.GateReason != "" {
		return &exitCodeError{code: exitGate, err: errors.New(out.GateReason)}
	}
	return nil
}

type localJSON struct {
	Status   string        `json:"status"`
	Model    string        `json:"model,omitempty"`
	Base     string        `json:"base"`
	Head     string        `json:"head"`
	Findings []ocr.Comment `json:"findings"`
	Gate     string        `json:"fail_on_severity_reached,omitempty"`
	// Reused counts the files not reviewed again: unchanged since the last run.
	Reused int `json:"reused_files"`
}

// fileStore keeps the reuse state of local runs in a file.
type fileStore string

func (f fileStore) Load() ([]byte, error) {
	data, err := os.ReadFile(string(f))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (f fileStore) Save(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o750); err != nil {
		return err
	}
	// A temp file of its own per run: worktrees share this directory, and two
	// runs may save at once. The last rename wins; both states are valid.
	tmp, err := os.CreateTemp(filepath.Dir(string(f)), filepath.Base(string(f))+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), string(f)); err != nil {
		return err
	}
	pruneStates(filepath.Dir(string(f)))
	return nil
}

// stateMaxAge is how long the state of a branch nobody reviews locally any
// more is kept. Branches are not tracked; an old state is merely dropped.
const stateMaxAge = 30 * 24 * time.Hour

// pruneStates removes state files, and temp files an interrupted Save left
// behind, that were not written for stateMaxAge.
func pruneStates(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "reuse-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > stateMaxAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func branchOr(branch, head string) string {
	if branch != "" {
		return branch
	}
	return head[:min(8, len(head))]
}
