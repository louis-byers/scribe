package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #138: gitChangedFiles returns absolute paths; gatherExtractFiles
// must not join them onto the project path again.
func TestGatherExtractFilesAcceptsAbsoluteChangedPaths(t *testing.T) {
	repo := t.TempDir()
	gitInitKB(t, repo)
	// A changed file OUTSIDE the step-5 doc dirs, so only step 4 can carry it.
	if err := os.MkdirAll(filepath.Join(repo, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes", "delta.md"), []byte("the delta"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "delta")

	changed := gitChangedFiles(repo, "", extractScanPatterns)
	var hit bool
	for _, c := range changed {
		if filepath.IsAbs(c) && strings.HasSuffix(c, filepath.Join("notes", "delta.md")) {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("precondition: gitChangedFiles must return the absolute path, got %v", changed)
	}
	entry := &ProjectEntry{Name: "proj", Path: repo}
	content, _ := gatherExtractFiles(t.TempDir(), entry, changed, filepath.Join(repo, "no-staging"), 10000, 100000)
	if !strings.Contains(content, "### CHANGED: notes/delta.md") || !strings.Contains(content, "the delta") {
		t.Fatalf("CHANGED section missing for an absolute changed path:\n%s", content)
	}
	// Repo-relative input keeps working.
	content, _ = gatherExtractFiles(t.TempDir(), entry, []string{"notes/delta.md"}, filepath.Join(repo, "no-staging"), 10000, 100000)
	if !strings.Contains(content, "### CHANGED: notes/delta.md") {
		t.Fatalf("CHANGED section missing for a relative changed path:\n%s", content)
	}
}

// Issue #131 (worktree half): nested checkouts — Claude Code worktrees,
// Worktrunk worktrees, submodules — are skipped by every project walk.
func TestProjectWalksSkipNestedCheckouts(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("README.md", "real")
	mk("docs/guide.md", "real")
	mk(".git/HEAD", "ref: refs/heads/main")
	// Claude Code worktree: .git is a FILE pointing at the main gitdir.
	mk(".claude/worktrees/feat/.git", "gitdir: ../../../.git/worktrees/feat")
	mk(".claude/worktrees/feat/README.md", "dup")
	mk(".claude/worktrees/feat/docs/guide.md", "dup")
	// Worktrunk worktree, same shape.
	mk(".worktrees/fix/.git", "gitdir: ../../.git/worktrees/fix")
	mk(".worktrees/fix/docs/guide.md", "dup")
	// Submodule: .git is a directory.
	mk("vendor/lib/.git/HEAD", "ref: refs/heads/main")
	mk("vendor/lib/README.md", "other repo")
	// .claude/research is NOT a checkout and must stay visible.
	mk(".claude/research/2026-01-01-topic.md", "research")

	assertClean := func(name string, got []string) {
		t.Helper()
		var own, dup int
		for _, g := range got {
			switch {
			case strings.Contains(g, "worktrees") || strings.Contains(g, "vendor/lib"):
				dup++
			default:
				own++
			}
		}
		if dup != 0 {
			t.Errorf("%s listed nested checkout files: %v", name, got)
		}
		if own == 0 {
			t.Errorf("%s lost the project's own files: %v", name, got)
		}
	}

	assertClean("collectKnowledgeFiles", collectKnowledgeFiles(root))
	dirs, err := findKnowledgeDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	assertClean("findKnowledgeDirs", dirs)
	assertClean("findFiles", findFiles(root, []string{"*.md"}))
	tree := assessSourceTree(root, 100)
	if strings.Contains(tree, "worktrees") || strings.Contains(tree, "vendor/lib") {
		t.Errorf("assessSourceTree listed nested checkout files:\n%s", tree)
	}
	if !strings.Contains(tree, "docs/guide.md") || !strings.Contains(tree, ".claude/research/2026-01-01-topic.md") {
		t.Errorf("assessSourceTree lost real files:\n%s", tree)
	}
	if nestedCheckout(root, root) {
		t.Error("the project root is never a nested checkout")
	}
}
