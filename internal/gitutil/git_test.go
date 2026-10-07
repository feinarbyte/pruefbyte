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

func TestSnapshotAndMergeBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "switch", "-qc", "feature")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	git(t, dir, "commit", "-qam", "change a")
	head := git(t, dir, "rev-parse", "HEAD")

	r := Repo{Dir: dir}
	ctx := context.Background()
	if mb, err := r.MergeBase(ctx, "main", "HEAD"); err != nil || mb != base {
		t.Fatalf("MergeBase = %s, %v; want %s", mb, err, base)
	}
	if target, err := r.DefaultTarget(ctx); err != nil || target != "main" {
		t.Errorf("DefaultTarget = %q, %v", target, err)
	}
	if _, err := r.MergeBase(ctx, "nope", "HEAD"); err == nil {
		t.Error("unknown target accepted")
	}
	if snap, err := r.Snapshot(ctx, head); err != nil || snap != head {
		t.Errorf("clean tree: Snapshot = %s, %v; want HEAD", snap, err)
	}

	// Unstaged, staged and untracked changes are all in the snapshot; ignored files are not.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("three\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("s\n"), 0o644)
	git(t, dir, "add", "staged.txt")
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("i\n"), 0o644)
	statusBefore := git(t, dir, "status", "--porcelain")
	snap, err := r.Snapshot(ctx, head)
	if err != nil || snap == head {
		t.Fatalf("Snapshot = %s, %v", snap, err)
	}
	if got := git(t, dir, "show", snap+":a.txt"); got != "three" {
		t.Errorf("a.txt in snapshot = %q", got)
	}
	files := git(t, dir, "ls-tree", "--name-only", snap)
	for _, want := range []string{"staged.txt", "new.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("%s missing from snapshot: %s", want, files)
		}
	}
	if strings.Contains(files, "ignored.txt") {
		t.Error("ignored file in snapshot")
	}
	if parent := git(t, dir, "rev-parse", snap+"^"); parent != head {
		t.Errorf("snapshot parent = %s, want HEAD", parent)
	}
	// Nothing about the user's repository changed.
	if now := git(t, dir, "rev-parse", "HEAD"); now != head {
		t.Error("HEAD moved")
	}
	if after := git(t, dir, "status", "--porcelain"); after != statusBefore {
		t.Errorf("status changed:\nbefore %q\nafter  %q", statusBefore, after)
	}
	if log, err := r.Log(ctx, base, head); err != nil || log != "change a" {
		t.Errorf("Log = %q, %v", log, err)
	}
}
