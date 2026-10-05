// Package gitutil runs the few git commands pruefbyte needs in the CI checkout.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Repo struct{ Dir string }

func (r Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*"}, args...)...)
	cmd.Dir = r.Dir
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
