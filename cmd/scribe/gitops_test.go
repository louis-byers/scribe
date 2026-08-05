package main

import (
	"strings"
	"testing"
)

// TestGitChangedFilesExcludesClaudeDir pins that tracked .claude/ tooling files
// (agent configs, commands, rules) are never pulled into the extraction set —
// they're tracked so git ls-files/diff would otherwise include them.
func TestGitChangedFilesExcludesClaudeDir(t *testing.T) {
	repo := initTestGitRepo(t, "Extract Tester")
	writeTestArticle(t, repo, "README.md", "# proj\n")
	writeTestArticle(t, repo, ".claude/rules/angular.md", "tooling config, not knowledge\n")
	writeTestArticle(t, repo, "docs/design.md", "real project knowledge\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	got := gitChangedFiles(repo, "", []string{"*.md", "*.txt"})

	for _, f := range got {
		if strings.Contains(f, "/.claude/") {
			t.Errorf(".claude tooling file leaked into extraction set: %s", f)
		}
	}
	found := false
	for _, f := range got {
		if strings.HasSuffix(f, "/docs/design.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected docs/design.md in the extraction set, got %v", got)
	}
}

func TestPullBeforeSyncEnabled_DefaultsTrue(t *testing.T) {
	if !pullBeforeSyncEnabled(nil) {
		t.Fatalf("nil cfg should default to enabled")
	}
	cfg := &ScribeConfig{}
	if !pullBeforeSyncEnabled(cfg) {
		t.Fatalf("unset pointer should default to enabled")
	}
}

func TestPullBeforeSyncEnabled_ExplicitFalse(t *testing.T) {
	f := false
	cfg := &ScribeConfig{Sync: SyncConfig{AlwaysPullBeforeSync: &f}}
	if pullBeforeSyncEnabled(cfg) {
		t.Fatalf("explicit false should disable")
	}
}

func TestPullRebase_NonRepoIsNoOp(t *testing.T) {
	ok, pulled, err := pullRebase(t.TempDir())
	if err != nil {
		t.Fatalf("non-repo should not error: %v", err)
	}
	if ok || pulled {
		t.Fatalf("non-repo should return ok=false, pulled=false")
	}
}

func TestCommitDebounced_DisabledWhenZero(t *testing.T) {
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 0}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected no debounce when CommitDebounceMinutes=0")
	}
}

func TestCommitDebounced_NoRepoTreatedAsOld(t *testing.T) {
	// A directory without a git HEAD returns a very large age so callers
	// proceed to commit on first run of a fresh KB.
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 30}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected non-repo path to fall through to commit, not debounce")
	}
}
