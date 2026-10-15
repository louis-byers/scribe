package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 2 of the 2026-10-15 audit plan: the trust layer must verify that
// "user-owned" files really are, and approval must not outrank the
// trust-locked source filters.

// gitInitKB turns a setupTrustKB root into a real repo so ls-files works.
func gitInitKB(t *testing.T, root string) {
	t.Helper()
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "config", "user.email", "test@example.com")
	gitRun(t, root, "config", "user.name", "Test")
	gitRun(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(localConfigName+"\nscripts/projects.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "seed")
}

func TestLocalOverridesRefusedWhenTrackedInTeamKB(t *testing.T) {
	repo := "team: true\ndefault_model: sonnet\n"
	root := setupTrustKB(t, repo, "default_model: haiku\n")
	gitInitKB(t, root)
	ensureConfigTrust(root) // records team=true

	// Untracked (gitignored) local file: honored, as documented.
	if cfg := loadConfig(root); cfg.DefaultModel != "haiku" {
		t.Fatalf("gitignored local file must apply, got %q", cfg.DefaultModel)
	}

	// A teammate force-adds it: every clone materializes it on pull.
	repoTrackedCache.Clear()
	gitRun(t, root, "add", "-f", localConfigName)
	gitRun(t, root, "commit", "-q", "-m", "smuggle")
	if cfg := loadConfig(root); cfg.DefaultModel != "sonnet" {
		t.Fatalf("tracked %s must be ignored in a team KB, got %q", localConfigName, cfg.DefaultModel)
	}
}

func TestLocalOverridesRefusedWhenSymlink(t *testing.T) {
	root := setupTrustKB(t, "default_model: sonnet\n", "")
	outside := filepath.Join(t.TempDir(), "evil.yaml")
	if err := os.WriteFile(outside, []byte("default_model: haiku\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, localConfigName)); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(root); cfg.DefaultModel != "sonnet" {
		t.Fatalf("symlinked %s must be ignored, got %q", localConfigName, cfg.DefaultModel)
	}
}

func TestLocalOverridesTrackedButSoloStillApply(t *testing.T) {
	root := setupTrustKB(t, "default_model: sonnet\n", "default_model: haiku\n")
	gitInitKB(t, root)
	repoTrackedCache.Clear()
	gitRun(t, root, "add", "-f", localConfigName)
	gitRun(t, root, "commit", "-q", "-m", "mine")
	// Solo KB: the user owns the whole repo; nothing to protect against.
	if cfg := loadConfig(root); cfg.DefaultModel != "haiku" {
		t.Fatalf("solo KB must still honor a tracked local file, got %q", cfg.DefaultModel)
	}
}

func TestAutoApproveIsTrustLocked(t *testing.T) {
	root := setupTrustKB(t, "team: true\n", "")
	ensureConfigTrust(root)
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("team: true\nsync:\n  auto_approve: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	if cfg.Sync.AutoApprove {
		t.Fatal("pushed sync.auto_approve: true applied despite the trust record")
	}
	// applyTo already reverted cfg, so diff the raw repo view.
	raw, _ := repoSensitiveView(root)
	if d := sensitiveDiff(loadTrustRecord(root).Sensitive, raw); len(d) != 1 || !strings.HasPrefix(d[0], "sync.auto_approve:") {
		t.Fatalf("diff must name the key: %v", d)
	}
}

func TestAutoApproveFalseKeepsOldRecordHash(t *testing.T) {
	// Records written before the field existed carry no key; a current
	// view with the default false must hash identically or every team
	// member would see phantom drift after upgrading.
	data, err := json.Marshal(sensitiveFrom(&ScribeConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sync_auto_approve") {
		t.Fatalf("false must be omitted from the snapshot: %s", data)
	}
}

func TestLoadManifestRefusesTrackedInTeamKB(t *testing.T) {
	root := setupTrustKB(t, "team: true\n", "")
	gitInitKB(t, root)
	ensureConfigTrust(root)
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"projects":{"/attacker/path":{"path":"/attacker/path","name":"x","status":"approved"}}}`
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(root); err != nil {
		t.Fatalf("untracked manifest must load: %v", err)
	}
	repoTrackedCache.Clear()
	gitRun(t, root, "add", "-f", "scripts/projects.json")
	gitRun(t, root, "commit", "-q", "-m", "smuggle")
	if _, err := loadManifest(root); err == nil || !strings.Contains(err.Error(), "tracked by git") {
		t.Fatalf("tracked manifest in a team KB must be refused, got err=%v", err)
	}
}

func TestExtractionRechecksSourceFilters(t *testing.T) {
	excluded := t.TempDir()
	allowed := t.TempDir()
	root := setupTrustKB(t, "sources:\n  exclude: [\""+excluded+"\"]\n", "")
	m := &Manifest{Projects: map[string]*ProjectEntry{
		excluded: {Path: excluded, Name: "ex"},
		allowed:  {Path: allowed, Name: "ok"},
	}}
	captureSyncLog(t)
	got := (&SyncCmd{}).projectsNeedingExtraction(root, m)
	if len(got) != 1 || got[0] != allowed {
		t.Fatalf("approved-but-excluded entry must not extract: %v", got)
	}
}
