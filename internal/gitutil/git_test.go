package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestShowFileAndEnsureCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, ".pruefbyte.yml"), []byte("review:\n  max_comments: 3\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, ".pruefbyte.yml"), []byte("review:\n  max_comments: 99\n"), 0o644)
	git(t, dir, "commit", "-qam", "head")
	head := git(t, dir, "rev-parse", "HEAD")

	r := Repo{Dir: dir}
	ctx := context.Background()
	data, found, err := r.ShowFile(ctx, base, ".pruefbyte.yml")
	if err != nil || !found || !strings.Contains(string(data), "3") {
		t.Fatalf("ShowFile at base = %q %v %v", data, found, err)
	}
	if _, found, err := r.ShowFile(ctx, base, "missing.yml"); err != nil || found {
		t.Errorf("missing file: found=%v err=%v", found, err)
	}
	if _, _, err := r.ShowFile(ctx, strings.Repeat("0", 40), ".pruefbyte.yml"); err == nil {
		t.Error("unknown commit should be an error")
	}
	if err := r.EnsureCommits(ctx, nil, base, head); err != nil {
		t.Errorf("EnsureCommits with present commits: %v", err)
	}
	if err := r.EnsureCommits(ctx, nil, strings.Repeat("1", 40)); err == nil {
		t.Error("EnsureCommits with an unknown commit and no remote should fail")
	}
}
