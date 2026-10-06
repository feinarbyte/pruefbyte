// Package gitutil runs the few git commands pruefbyte needs in the CI checkout.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct{ Dir string }

func (r Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.runEnv(ctx, nil, args...)
}

func (r Repo) runEnv(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*"}, args...)...)
	cmd.Dir = r.Dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ShowFile returns path at rev, or (nil, false) if it does not exist there.
func (r Repo) ShowFile(ctx context.Context, rev, path string) ([]byte, bool, error) {
	if _, err := r.run(ctx, "cat-file", "-e", rev+":"+path); err != nil {
		if _, cerr := r.run(ctx, "cat-file", "-e", rev+"^{commit}"); cerr != nil {
			return nil, false, fmt.Errorf("commit %.8s is not in the clone (set GIT_DEPTH: 0): %w", rev, cerr)
		}
		return nil, false, nil
	}
	out, err := r.run(ctx, "show", rev+":"+path)
	return out, err == nil, err
}

// EnsureCommits fetches any of the given commits missing from the clone.
func (r Repo) EnsureCommits(ctx context.Context, fetchRefs []string, shas ...string) error {
	missing := func() []string {
		var m []string
		for _, s := range shas {
			if _, err := r.run(ctx, "cat-file", "-e", s+"^{commit}"); err != nil {
				m = append(m, s)
			}
		}
		return m
	}
	m := missing()
	if len(m) == 0 {
		return nil
	}
	// Shallow clones: deepen to full history; then try fetching the commits and MR refs directly.
	if out, _ := r.run(ctx, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(string(out)) == "true" {
		_, _ = r.run(ctx, "fetch", "--quiet", "--unshallow", "origin")
	}
	for _, ref := range append(missing(), fetchRefs...) {
		if len(missing()) == 0 {
			break
		}
		_, _ = r.run(ctx, "fetch", "--quiet", "origin", ref)
	}
	if m = missing(); len(m) > 0 {
		return fmt.Errorf("commits %s are not available in %s; use GIT_DEPTH: 0 in the CI job", strings.Join(m, ", "), r.Dir)
	}
	return nil
}

func (r Repo) revParse(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

// DefaultTarget guesses the branch merge requests go into: origin's HEAD, else
// origin/main or origin/master, else a local main or master.
func (r Repo) DefaultTarget(ctx context.Context) (string, error) {
	if out, err := r.run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref, nil
		}
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := r.revParse(ctx, ref); err == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("cannot tell the target branch: pass --target (e.g. origin/main)")
}

// MergeBase returns the commit a merge request from HEAD into target would be
// reviewed from, like GitLab's diff base.
func (r Repo) MergeBase(ctx context.Context, target, head string) (string, error) {
	if _, err := r.revParse(ctx, target); err != nil {
		return "", fmt.Errorf("target %q is not a commit in %s (fetch it, or pass --target)", target, r.Dir)
	}
	out, err := r.run(ctx, "merge-base", target, head)
	if err != nil {
		return "", fmt.Errorf("no common ancestor of %s and %s: %w", target, head, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Head returns the commit HEAD points to.
func (r Repo) Head(ctx context.Context) (string, error) {
	sha, err := r.revParse(ctx, "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s has no commits yet", r.Dir)
	}
	return sha, nil
}

// Snapshot returns a commit holding the working tree as it is now: tracked
// changes, staged or not, plus untracked files that are not ignored. It uses a
// throwaway copy of the index, so the real index, HEAD and branches stay
// untouched; the commit is unreferenced and git eventually prunes it. Without
// changes it returns HEAD itself.
func (r Repo) Snapshot(ctx context.Context) (string, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "pruefbyte-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	index := filepath.Join(tmp, "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	// Starting from a copy of the real index keeps its stat data, so `git add`
	// only rehashes files that changed, and its skip-worktree bits, so files
	// outside a sparse checkout are not recorded as deleted.
	if err := r.copyIndex(ctx, index); err != nil {
		if _, err := r.runEnv(ctx, env, "read-tree", head); err != nil {
			return "", err
		}
	}
	if _, err := r.runEnv(ctx, env, "add", "--all", "--", "."); err != nil {
		return "", err
	}
	out, err := r.runEnv(ctx, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if out, err := r.run(ctx, "rev-parse", head+"^{tree}"); err == nil && strings.TrimSpace(string(out)) == tree {
		return head, nil
	}
	out, err = r.runEnv(ctx, []string{
		"GIT_AUTHOR_NAME=pruefbyte", "GIT_AUTHOR_EMAIL=pruefbyte@localhost",
		"GIT_COMMITTER_NAME=pruefbyte", "GIT_COMMITTER_EMAIL=pruefbyte@localhost",
	}, "commit-tree", tree, "-p", head, "-m", "pruefbyte local review snapshot")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// copyIndex copies the repository's index file to dst.
func (r Repo) copyIndex(ctx context.Context, dst string) error {
	out, err := r.run(ctx, "rev-parse", "--git-path", "index")
	if err != nil {
		return err
	}
	src := filepath.FromSlash(strings.TrimSpace(string(out)))
	if !filepath.IsAbs(src) {
		src = filepath.Join(r.Dir, src)
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil { //nolint:gosec // G703: dst is in pruefbyte's own temp dir
		return err
	}
	// git's racy-clean check compares entries with the index file's mtime.
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// Log returns the subjects and bodies of the commits in base..head, oldest first.
func (r Repo) Log(ctx context.Context, base, head string) (string, error) {
	out, err := r.run(ctx, "log", "--reverse", "--format=%B", base+".."+head)
	return strings.TrimSpace(string(out)), err
}

// Toplevel returns the root of the working tree containing r.Dir.
func (r Repo) Toplevel(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.FromSlash(strings.TrimSpace(string(out))), nil
}

// Branch returns the current branch name, or "" when HEAD is detached.
func (r Repo) Branch(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	return strings.TrimSpace(string(out)), err
}
