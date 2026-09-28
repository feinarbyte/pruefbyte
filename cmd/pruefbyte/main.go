// Command pruefbyte reviews GitLab merge requests with OpenCodeReview and posts
// the findings as inline discussions from a bot account.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"pruefbyte/internal/ci"
	"pruefbyte/internal/config"
	"pruefbyte/internal/gitlab"
	"pruefbyte/internal/gitutil"
	"pruefbyte/internal/ocr"
	"pruefbyte/internal/review"
)

var version = "dev"

// Exit codes.
const (
	exitError = 1
	exitGate  = 3
)

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := rootCmd().ExecuteContext(ctx)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "[pruefbyte] error:", err)
	var ec *exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}
	os.Exit(exitError)
}

type globalFlags struct {
	configPath string
	repoConfig bool
}

func rootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "pruefbyte",
		Short:         "GitLab merge request reviews powered by OpenCodeReview",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", os.Getenv("PRUEFBYTE_CONFIG"), "global config file (env PRUEFBYTE_CONFIG)")
	root.PersistentFlags().BoolVar(&g.repoConfig, "repo-config", true, "read "+config.RepoConfigFile+" from the merge request's base commit")
	root.AddCommand(reviewCmd(g), configCmd(g), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), "pruefbyte", version) },
	})
	return root
}

// baseConfig is defaults < global file < env. The repo file is layered in later,
// below env, once the base commit is known.
func baseConfig(g *globalFlags) (config.Config, error) {
	cfg := config.Default()
	if err := cfg.LoadGlobalFile(g.configPath); err != nil {
		return cfg, err
	}
	return cfg, cfg.ApplyEnv(os.LookupEnv)
}

func configCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect configuration"}
	var repoFile string
	printCmd := &cobra.Command{
		Use:   "print",
		Short: "Print the effective configuration (secrets are only referenced by env var name)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Default()
			if err := cfg.LoadGlobalFile(g.configPath); err != nil {
				return err
			}
			if repoFile != "" {
				data, err := os.ReadFile(repoFile)
				if err != nil {
					return err
				}
				if err := cfg.ApplyRepo(data); err != nil {
					return err
				}
			}
			if err := cfg.ApplyEnv(os.LookupEnv); err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), cfg.YAML())
			return cfg.Validate()
		},
	}
	printCmd.Flags().StringVar(&repoFile, "repo-file", "", "also apply this repository config file")
	cmd.AddCommand(printCmd)
	return cmd
}

type reviewFlags struct {
	gitlabURL   string
	project     string
	mr          int64
	repoDir     string
	dryRun      bool
	artifactDir string
}

func reviewCmd(g *globalFlags) *cobra.Command {
	f := &reviewFlags{}
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review the current merge request and post inline comments",
		Long: `Review a merge request with OpenCodeReview and post the findings as inline
discussions. Inside a GitLab merge request pipeline everything is read from the
CI variables; outside CI pass --project and --mr and run from a clone of the repo.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runReview(cmd.Context(), g, f) },
	}
	cmd.Flags().StringVar(&f.gitlabURL, "gitlab-url", "", "GitLab URL (default: gitlab.url or CI_SERVER_URL)")
	cmd.Flags().StringVar(&f.project, "project", "", "project ID or path (default: CI_MERGE_REQUEST_PROJECT_ID)")
	cmd.Flags().Int64Var(&f.mr, "mr", 0, "merge request IID (default: CI_MERGE_REQUEST_IID)")
	cmd.Flags().StringVar(&f.repoDir, "repo", "", "path of the git clone (default: CI_PROJECT_DIR or cwd)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print the comments instead of posting them")
	cmd.Flags().StringVar(&f.artifactDir, "artifact-dir", os.Getenv("PRUEFBYTE_ARTIFACT_DIR"), "directory to keep ocr-result.json in (env PRUEFBYTE_ARTIFACT_DIR)")
	return cmd
}

func runReview(ctx context.Context, g *globalFlags, f *reviewFlags) error {
	base, err := baseConfig(g)
	if err != nil {
		return err
	}
	mrc := ci.FromEnv(os.Getenv)
	if base.GitLab.URL != "" {
		mrc.ServerURL = base.GitLab.URL
	}
	if f.gitlabURL != "" {
		mrc.ServerURL = f.gitlabURL
	}
	if f.project != "" {
		mrc.Project = f.project
		mrc.HeadSHA = "" // not the pipeline's MR
	}
	if f.mr != 0 {
		mrc.MRIID = f.mr
		mrc.HeadSHA = ""
	}
	if f.repoDir != "" {
		mrc.RepoDir = f.repoDir
	}
	if mrc.RepoDir, err = filepath.Abs(mrc.RepoDir); err != nil {
		return err
	}
	if err := mrc.Validate(); err != nil {
		return err
	}

	token := os.Getenv(base.GitLab.TokenEnv)
	if token == "" {
		return fmt.Errorf("GitLab token missing: set %s to the bot account's personal access token", base.GitLab.TokenEnv)
	}
	apiKey := os.Getenv(base.LLM.APIKeyEnv)

	client, err := gitlab.New(strings.TrimRight(mrc.ServerURL, "/")+"/api/v4", token, mrc.Project, mrc.MRIID)
	if err != nil {
		return err
	}
	var api gitlab.API = client
	if f.dryRun {
		api = gitlab.DryRun{API: client, Out: os.Stdout}
	}

	runner, err := ocr.NewRunner(base.OCR.Binary, os.Stderr)
	if err != nil {
		return err
	}
	defer runner.Close()
	v, err := runner.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[pruefbyte] %s, %s\n", version, strings.SplitN(v, "\n", 2)[0])

	repo := gitutil.Repo{Dir: mrc.RepoDir}
	deps := review.Deps{
		GitLab: api,
		OCR:    runner,
		Log:    os.Stderr,
		LoadConfig: func(ctx context.Context, baseSHA string) (config.Config, error) {
			cfg := config.Default()
			if err := cfg.LoadGlobalFile(g.configPath); err != nil {
				return cfg, err
			}
			if g.repoConfig {
				data, found, err := repo.ShowFile(ctx, baseSHA, config.RepoConfigFile)
				if err != nil {
					return cfg, err
				}
				if found {
					fmt.Fprintf(os.Stderr, "[pruefbyte] using %s from %.8s\n", config.RepoConfigFile, baseSHA)
					if err := cfg.ApplyRepo(data); err != nil {
						return cfg, err
					}
				}
			}
			if err := cfg.ApplyEnv(os.LookupEnv); err != nil {
				return cfg, err
			}
			if err := cfg.Validate(); err != nil {
				return cfg, err
			}
			if apiKey == "" && cfg.LLM.Provider != "bedrock" {
				fmt.Fprintf(os.Stderr, "[pruefbyte] warning: %s is empty; ocr falls back to the provider's own env var\n", cfg.LLM.APIKeyEnv)
			}
			return cfg, runner.Configure(ctx, cfg.LLM, apiKey, cfg.OCR.Language)
		},
		EnsureCommits: func(ctx context.Context, shas ...string) error {
			return repo.EnsureCommits(ctx, []string{fmt.Sprintf("refs/merge-requests/%d/head", mrc.MRIID)}, shas...)
		},
		Redact: func(s string) string {
			for _, secret := range []string{apiKey, token} {
				if len(secret) >= 4 {
					s = strings.ReplaceAll(s, secret, "***")
				}
			}
			return s
		},
	}
	if f.artifactDir != "" {
		deps.SaveResult = func(raw []byte) error {
			if err := os.MkdirAll(f.artifactDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(f.artifactDir, "ocr-result.json"), raw, 0o644)
		}
	}

	out, err := review.Run(ctx, deps, review.Options{RepoDir: mrc.RepoDir, ExpectedHeadSHA: mrc.HeadSHA})
	if out != nil && out.Skipped == "" {
		s := out.Stats
		fmt.Fprintf(os.Stderr, "[pruefbyte] findings=%d posted=%d duplicates=%d outside_diff=%d over_limit=%d failed=%d resolved=%d\n",
			s.Findings, s.Posted, s.Duplicates, s.Unplaced, s.Overflow, s.Failed, s.Resolved)
	}
	if err != nil {
		return err
	}
	if out.GateFailed {
		return &exitCodeError{code: exitGate, err: errors.New(out.GateReason)}
	}
	return nil
}
