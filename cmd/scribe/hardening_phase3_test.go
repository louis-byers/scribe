package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 3A of the 2026-10-15 audit plan: repo-controlled names and CLI
// inputs that become path segments.

func TestSafePathSegment(t *testing.T) {
	for _, ok := range []string{"general", "my-project", "a.b", "Spaced Name", "ünïcode"} {
		if !safePathSegment(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`, "/abs", "x\x00y"} {
		if safePathSegment(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestConfigDropsNamesThatAreNotSegments(t *testing.T) {
	root := setupTrustKB(t,
		"kb_name: \"../docs\"\ndomains: [\"ok\", \"../../outside\"]\nmeta:\n  rolling_targets: [\"learnings\", \"../../../tmp/x\"]\n", "")
	cfg := loadConfig(root)
	if cfg.KBName != "" || kbName(root) != filepath.Base(root) {
		t.Errorf("kb_name with a separator must fall back to the directory name, got %q / %q", cfg.KBName, kbName(root))
	}
	if strings.Join(cfg.Domains, ",") != "ok" {
		t.Errorf("domains = %v, want only the plain name", cfg.Domains)
	}
	if strings.Join(cfg.Meta.RollingTargets, ",") != "learnings" {
		t.Errorf("rolling_targets = %v, want only the plain name", cfg.Meta.RollingTargets)
	}
	if allowedDomainsForRoot(root)["../../outside"] {
		t.Error("traversal domain reached the rolling-append allowlist")
	}
}

func TestWriteRollingRefusesProjectEscape(t *testing.T) {
	root := testKBRoot(t)
	w := &WriteCmd{Rolling: "learnings", Project: "../../outside", Title: "x"}
	err := w.runRolling(root, "body")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected containment error, got %v", err)
	}
	w = &WriteCmd{Rolling: "learnings", Project: "..", Title: "x"}
	if err := w.runRolling(root, "body"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected containment error for '..', got %v", err)
	}
}

func TestValidateDropNames(t *testing.T) {
	if err := validateDropNames("my-note", "scriptorium"); err != nil {
		t.Fatalf("plain names refused: %v", err)
	}
	if err := validateDropNames("../../note", "kb"); err == nil {
		t.Fatal("slug traversal accepted")
	}
	if err := validateDropNames("note", "../other"); err == nil {
		t.Fatal("kb-name traversal accepted")
	}
}

func TestMarkerImagesStayInsideOutDir(t *testing.T) {
	base := t.TempDir()
	out := filepath.Join(base, "marker-out")
	kb := filepath.Join(base, "kb")
	for _, d := range []string{out, kb} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// One legitimate image inside the marker output, one reference that
	// points above it.
	if err := os.WriteFile(filepath.Join(out, "fig1.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret.png"), []byte("leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	md := "![a](fig1.png)\n![b](../secret.png)\n"
	preserveMarkerImages(md, out, kb, "doc")
	if _, err := os.Stat(filepath.Join(kb, "raw", "assets", "doc", "fig1.png")); err != nil {
		t.Errorf("legitimate image not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kb, "raw", "assets", "doc", "secret.png")); err == nil {
		t.Error("image reference above the marker output dir was copied into the KB")
	}
}

// --- Phase 3B: executor scoping -----------------------------------------

func writePage(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateOverwriteRequiresSourceOwnership(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw", "articles", "my-article.md")
	owned := writePage(t, root, "wiki/owned.md", "---\ntitle: Owned\nsources: [\""+raw+"\"]\n---\nold\n")
	byBase := writePage(t, root, "wiki/by-base.md", "---\ntitle: B\nsources:\n  - raw/articles/my-article.md\n---\nold\n")
	foreign := writePage(t, root, "wiki/foreign.md", "---\ntitle: F\nsources: [\"/elsewhere/other.md\"]\n---\nold\n")
	curated := writePage(t, root, "wiki/curated.md", "---\ntitle: C\n---\nold\n")

	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "create", Path: "wiki/owned.md", Content: "new"},
		{Op: "create", Path: "wiki/by-base.md", Content: "new"},
		{Op: "create", Path: "wiki/foreign.md", Content: "new"},
		{Op: "create", Path: "wiki/curated.md", Content: "new"},
		{Op: "create", Path: "wiki/fresh.md", Content: "new"},
	}}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, OverwriteSource: raw})
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(owned) != "new" || read(byBase) != "new" {
		t.Errorf("pages owned by the source must be replaced: owned=%q byBase=%q", read(owned), read(byBase))
	}
	if read(foreign) == "new" || read(curated) == "new" {
		t.Errorf("pages from another source / without sources must survive: foreign=%q curated=%q", read(foreign), read(curated))
	}
	if read(filepath.Join(root, "wiki", "fresh.md")) != "new" {
		t.Error("a brand-new page must still be created")
	}
	if len(res.Errors) != 2 {
		t.Errorf("expected exactly the two refusals, got %v", res.Errors)
	}
}

func TestAllowedDomainsConfineProjectWrites(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "create", Path: "projects/hot/notes.md", Content: "in scope"},
		{Op: "create", Path: "projects/other/notes.md", Content: "out of scope"},
		{Op: "create", Path: "wiki/page.md", Content: "not a project path"},
	}}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowedDomains: []string{"hot"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "hot", "notes.md")); err != nil {
		t.Error("in-scope domain write refused")
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "other", "notes.md")); err == nil {
		t.Error("out-of-scope domain write went through")
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "page.md")); err != nil {
		t.Error("non-project path must be unaffected by AllowedDomains")
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "outside this run's scope") {
		t.Errorf("errors = %v", res.Errors)
	}
}

func TestRollingAppendScoping(t *testing.T) {
	root := t.TempDir()
	meta := []MetaAction{{Op: "rolling_memory_append", Domain: "general", Target: "learnings", Content: "x"}}
	res, _ := applyWikiActions(root, WikiActionEnvelope{Meta: meta}, ApplyOptions{DisallowMetaRolling: true})
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "not permitted") {
		t.Errorf("DisallowMetaRolling: %v", res.Errors)
	}
	res, _ = applyWikiActions(root, WikiActionEnvelope{Meta: meta}, ApplyOptions{AllowedDomains: []string{"personal"}})
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "outside this run's scope") {
		t.Errorf("AllowedDomains on rolling append: %v", res.Errors)
	}
	res, _ = applyWikiActions(root, WikiActionEnvelope{Meta: meta}, ApplyOptions{AllowedDomains: []string{"general"}})
	if len(res.Errors) != 0 {
		t.Errorf("in-scope rolling append refused: %v", res.Errors)
	}
}

func TestModelSessionLogAppendIsIgnored(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Meta: []MetaAction{{Op: "sessions_log_append", SessionID: "someone-elses-session"}}}
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("op must be accepted silently: err=%v errors=%v", err, res.Errors)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "_sessions_log.json")); err == nil {
		t.Fatal("a model-supplied session id was recorded")
	}
}
