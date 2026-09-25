package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fillSessionMessages gives a fixture session enough substance to clear the
// pre-filter's mechanical gate (>=1 user message and >=500 chars, or
// filterVerdict reads "empty"). insertFixtureSession sets the denormalized
// sessions.message_count but inserts no messages rows, and both status and
// the miner compute their stats from the messages JOIN — so a fixture
// without this is a session the miner would skip, and counting it as
// pending is exactly the mismatch these tests exist to pin.
func fillSessionMessages(t *testing.T, db *sql.DB, rowID int64) {
	t.Helper()
	body := strings.Repeat("substantive discussion content. ", 40)
	insertFixtureMessage(t, db, rowID, "user", "opening question "+body, false)
	insertFixtureMessage(t, db, rowID, "assistant", "reply "+body, false)
	insertFixtureMessage(t, db, rowID, "user", "follow-up question", false)
	insertFixtureMessage(t, db, rowID, "user", "third question", false)
}

// TestCountScopedPendingSessions verifies the backlog counts only sessions
// whose project has an APPROVED manifest entry — not the whole global
// ccrider DB (issue #27 item 4). A fresh KB with zero approved projects
// must report zero pending, not the machine's entire session pile.
func TestCountScopedPendingSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()

	approved := filepath.Join(t.TempDir(), "Projects", "approved")
	pending := filepath.Join(t.TempDir(), "Projects", "pending")

	// Manifest: one approved project, one still-pending project.
	writeStatusManifest(t, root, map[string]map[string]string{
		"approved": {"path": approved, "domain": "general"}, // empty status = approved
		"pending":  {"path": pending, "domain": "general", "status": statusPending},
	})

	// Sessions across approved / pending / unknown / empty-cwd projects.
	fillSessionMessages(t, db, insertFixtureSession(t, db, "s-appr-1", approved, 50, "", "", "in approved"))
	insertFixtureSession(t, db, "s-appr-2", approved, 50, "", "", "in approved, already mined")
	insertFixtureSession(t, db, "s-pending", pending, 50, "", "", "in pending project")
	insertFixtureSession(t, db, "s-unknown", "/somewhere/else", 50, "", "", "no manifest entry")
	insertFixtureSession(t, db, "s-empty", "", 50, "", "", "no cwd")

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	processed := map[string]struct{}{"s-appr-2": {}}
	got, ok := countScopedPendingSessions(root, cfg, processed)
	if !ok {
		t.Fatal("countScopedPendingSessions returned ok=false")
	}
	// Only s-appr-1 counts: s-appr-2 is processed, s-pending is unapproved,
	// s-unknown has no entry, s-empty has no cwd.
	if got != 1 {
		t.Errorf("pending = %d, want 1 (only the unprocessed approved-project session)", got)
	}
}

// TestCountScopedPendingSessions_ZeroApproved is the headline #27 case:
// a KB with no approved projects must not claim the global session pile.
func TestCountScopedPendingSessions_ZeroApproved(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()
	writeStatusManifest(t, root, map[string]map[string]string{}) // empty manifest

	insertFixtureSession(t, db, "g1", "/some/proj", 99, "", "", "x")
	insertFixtureSession(t, db, "g2", "/other/proj", 99, "", "", "y")

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	got, ok := countScopedPendingSessions(root, cfg, map[string]struct{}{})
	if !ok {
		t.Fatal("ok=false")
	}
	if got != 0 {
		t.Errorf("pending = %d, want 0 — a KB with no approved projects owns no sessions", got)
	}
}

// TestPendingQueueSummary covers pendingQueueSummary's Hot/Normal/aged
// classification for `scribe status` (issue #22): missing queue file
// reports ok=false, an empty file reports ok=true with zero counts, and a
// mix of Hot/Normal/aged entries lands in the right buckets.
func TestPendingQueueSummary(t *testing.T) {
	cfg := PriorityLanesConfig{HotThreshold: 90, AgeDays: 7}

	t.Run("missing queue file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if ok {
			t.Errorf("ok = true for a missing queue file, want false")
		}
		if hot != 0 || normal != 0 || aged != 0 {
			t.Errorf("counts = (%d, %d, %d), want zeros", hot, normal, aged)
		}
	})

	t.Run("empty queue file reads the same as a missing one", func(t *testing.T) {
		// scanPendingEntries returns a nil slice when it finds zero lines
		// (an empty `var out []pendingEntry`, never appended to), which is
		// indistinguishable from peekPendingEntries' os.Open-failed nil —
		// so an existing-but-empty file and a missing file both read as
		// ok=false here. Harmless at the status.go call site either way:
		// it only ever prints when ok && hot+normal>0, so both cases
		// suppress the row identically.
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		scribeDir := filepath.Join(xdg, "scribe")
		if err := os.MkdirAll(scribeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scribeDir, "pending-sessions.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if ok {
			t.Error("ok = true for an empty file — scanPendingEntries returns nil on zero lines, same as a missing file")
		}
		if hot != 0 || normal != 0 || aged != 0 {
			t.Errorf("counts = (%d, %d, %d), want zeros", hot, normal, aged)
		}
	})

	t.Run("mixed hot/normal/aged entries", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		scribeDir := filepath.Join(xdg, "scribe")
		if err := os.MkdirAll(scribeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		agedTS := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339) // > AgeDays=7
		freshTS := now.Add(-1 * time.Hour).Format(time.RFC3339)
		content := "s-hot\t95\t50\t" + freshTS + "\n" + // score-hot
			"s-normal\t40\t50\t" + freshTS + "\n" + // normal, fresh
			"s-aged\t40\t50\t" + agedTS + "\n" + // aged into hot
			"s-legacy\n" // bare-ID legacy shape: LegacyUnknownAge -> hot, counts as aged too
		if err := os.WriteFile(filepath.Join(scribeDir, "pending-sessions.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if !ok {
			t.Fatal("ok = false")
		}
		if hot != 3 {
			t.Errorf("hot = %d, want 3 (s-hot, s-aged, s-legacy)", hot)
		}
		if normal != 1 {
			t.Errorf("normal = %d, want 1 (s-normal)", normal)
		}
		if aged != 2 {
			t.Errorf("aged = %d, want 2 (s-aged, s-legacy — both promoted by age, not by score)", aged)
		}
	})
}

// writeStatusManifest writes scripts/projects.json for a test KB.
func writeStatusManifest(t *testing.T, root string, projects map[string]map[string]string) {
	t.Helper()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "projects.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCountScopedPendingSessionsExcludesUnminable pins "pending" to mean
// "will actually be mined". An approved manifest entry answers only
// "does this path belong to an approved project"; admission additionally
// asks sessionDropReason. A session that satisfies the first and fails
// the second used to be counted forever while the miner refused it on
// every run, giving the backlog a floor it could never reach.
//
// The KB guard is the cheapest drop reason to construct, and the one that
// bites in practice: sessions run inside a KB checkout are never mined
// (KBs must not harvest themselves), yet the KB can be an approved project.
func TestCountScopedPendingSessionsExcludesUnminable(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()

	minable := filepath.Join(t.TempDir(), "Projects", "minable")
	// An approved project that is itself a scribe KB — approved, but the
	// miner drops every session in it via sessionInKB.
	kbProject := filepath.Join(t.TempDir(), "Projects", "someKB")
	if err := os.MkdirAll(kbProject, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbProject, "scribe.yaml"), []byte("owner_name: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeStatusManifest(t, root, map[string]map[string]string{
		"minable": {"path": minable, "domain": "general"},
		"someKB":  {"path": kbProject, "domain": "general"},
	})

	fillSessionMessages(t, db, insertFixtureSession(t, db, "s-minable", minable, 50, "", "", "will be mined"))
	// Deliberately ALSO mechanically rich: the KB guard must be what
	// excludes it, not an incidentally empty fixture.
	fillSessionMessages(t, db, insertFixtureSession(t, db, "s-in-kb", kbProject, 50, "", "", "approved but never admissible"))

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	got, ok := countScopedPendingSessions(root, cfg, map[string]struct{}{})
	if !ok {
		t.Fatal("countScopedPendingSessions returned ok=false")
	}
	if got != 1 {
		t.Errorf("pending = %d, want 1: the in-KB session is approved but unminable, so it must not be counted", got)
	}
	// Guard the composition: sessionDropReason alone is permissive about
	// unseen projects, so it must not be allowed to re-admit an unenrolled
	// path and undo #27.
	fillSessionMessages(t, db, insertFixtureSession(t, db, "s-unknown", "/somewhere/else/entirely", 50, "", "", "no manifest entry"))
	got, _ = countScopedPendingSessions(root, cfg, map[string]struct{}{})
	if got != 1 {
		t.Errorf("pending = %d, want 1: an unenrolled project must stay uncounted (#27)", got)
	}
}

// TestCountScopedPendingSessionsExcludesThin pins the mechanical half of
// "pending means the miner would admit it". sessionDropReason covers
// in-a-KB / out-of-scope / pending-approval, but the thin-session gate
// lives in preFilterSessions, so a session in a fully approved, in-scope
// project with no user turn still has to be excluded.
//
// Agent-only runs are the real case: a subagent or hook-spawned session
// records assistant turns and no user message, is refused by the miner
// every run, and — because it is never admitted — never reaches the
// marking that would otherwise retire it. Counting it as pending gives
// the backlog a floor.
func TestCountScopedPendingSessionsExcludesThin(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()
	proj := filepath.Join(t.TempDir(), "Projects", "approved")

	writeStatusManifest(t, root, map[string]map[string]string{
		"approved": {"path": proj, "domain": "general"},
	})

	// Substantial: counts.
	fillSessionMessages(t, db, insertFixtureSession(t, db, "s-rich", proj, 50, "", "", "real work"))

	// Agent-only: assistant turns, no user turn. filterVerdict = "empty".
	agentOnly := insertFixtureSession(t, db, "s-agent-only", proj, 2, "", "", "subagent run")
	insertFixtureMessage(t, db, agentOnly, "assistant", strings.Repeat("tool output. ", 200), false)
	insertFixtureMessage(t, db, agentOnly, "assistant", strings.Repeat("more output. ", 200), false)

	// Short back-and-forth under both thresholds. filterVerdict = "thin".
	short := insertFixtureSession(t, db, "s-thin", proj, 2, "", "", "quick question")
	insertFixtureMessage(t, db, short, "user", "hi", false)
	insertFixtureMessage(t, db, short, "assistant", "hello", false)

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	got, ok := countScopedPendingSessions(root, cfg, map[string]struct{}{})
	if !ok {
		t.Fatal("countScopedPendingSessions returned ok=false")
	}
	if got != 1 {
		t.Errorf("pending = %d, want 1: only the substantial session is minable; the agent-only and thin ones are refused by the pre-filter on every run", got)
	}
}
