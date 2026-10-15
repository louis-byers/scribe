package main

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Follow-up review of the 2026-10-15 audit fixes (Amp round 3): gaps the
// reviewer found in the first implementation.

func TestLoadManifestRefusesTrackedOnFreshTeamClone(t *testing.T) {
	// No trust record yet: a fresh clone before ensureConfigTrust runs.
	root := setupTrustKB(t, "team: true\n", "")
	gitInitKB(t, root)
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"projects":{"/attacker/path":{"path":"/attacker/path","name":"x","status":"approved"}}}`
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-f", "scripts/projects.json")
	gitRun(t, root, "commit", "-q", "-m", "smuggle")
	repoTrackedCache.Clear()
	if !teamKB(root, nil) {
		t.Fatal("teamKB must read team: true from the repo scribe.yaml when no trust record exists")
	}
	if _, err := loadManifest(root); err == nil || !strings.Contains(err.Error(), "tracked by git") {
		t.Fatalf("tracked manifest on a fresh team clone must be refused, got err=%v", err)
	}
}

func TestInvalidateRepoTrackedCache(t *testing.T) {
	root := setupTrustKB(t, "team: true\n", "")
	gitInitKB(t, root)
	repoTrackedCache.Clear()
	if repoFileTracked(root, localConfigName) {
		t.Fatal("local config starts untracked")
	}
	if err := os.WriteFile(filepath.Join(root, localConfigName), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-f", localConfigName)
	gitRun(t, root, "commit", "-q", "-m", "smuggle")
	if repoFileTracked(root, localConfigName) {
		t.Fatal("precondition: the memoized answer should still say untracked")
	}
	invalidateRepoTrackedCache()
	if !repoFileTracked(root, localConfigName) {
		t.Fatal("after invalidation the tracked file must be seen")
	}
}

func TestLogAppendRefusesSymlinkedLog(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.md")
	if err := os.WriteFile(victim, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "log.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	err := applyMetaLogAppend(root, MetaAction{Op: "log_append", Line: "injected"}, ApplyOptions{})
	if err == nil {
		t.Fatal("log_append through a symlink must fail")
	}
	if b, _ := os.ReadFile(victim); string(b) != "mine\n" {
		t.Fatalf("victim file was modified: %q", b)
	}
}

func TestDrainFileInboxRefusesSymlinkedInboxDir(t *testing.T) {
	root := setupTrustKB(t, "", "")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "host.md"), []byte("host file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "raw", "inbox")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := drainFileInbox(root); err == nil || !strings.Contains(err.Error(), "outside the KB root") {
		t.Fatalf("symlinked inbox dir must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "host.md")); err != nil {
		t.Fatal("host file must be untouched")
	}
}

func TestUpdateFrontmatterCannotGrantOwnership(t *testing.T) {
	root := t.TempDir()
	raw := filepath.Join(root, "raw", "articles", "mine.md")
	page := writePage(t, root, "wiki/foreign.md", "---\ntitle: Foreign\nsources: [/elsewhere/theirs.md]\n---\noriginal")
	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "update_frontmatter", Path: "wiki/foreign.md", Frontmatter: map[string]any{"sources": []any{raw}, "tags": []any{"x"}}},
		{Op: "create", Path: "wiki/foreign.md", Content: "overwritten"},
	}}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, OverwriteSource: raw})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(page)
	if strings.Contains(string(b), "overwritten") || strings.Contains(string(b), "mine.md") {
		t.Fatalf("sources rewrite must not unlock the overwrite:\n%s", b)
	}
	if !strings.Contains(string(b), "tags") {
		t.Errorf("non-ownership keys in the same update must still apply:\n%s", b)
	}
	if len(res.Errors) != 1 {
		t.Errorf("expected exactly the create refusal, got %v", res.Errors)
	}
}

func TestReadBodyBoundedRejectsOversize(t *testing.T) {
	big := bytes.Repeat([]byte("a"), int(maxHTTPBodyBytes)+1)
	if _, err := readBodyBounded(bytes.NewReader(big)); err == nil {
		t.Fatal("oversize body must be an error, not a truncated success")
	}
	small := []byte("ok")
	got, err := readBodyBounded(bytes.NewReader(small))
	if err != nil || string(got) != "ok" {
		t.Fatalf("small body: got %q err=%v", got, err)
	}
}

func TestHostedClientRefusesSchemeDowngrade(t *testing.T) {
	c := hostedHTTPClient()
	first := &http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com"}}
	next := &http.Request{URL: &url.URL{Scheme: "http", Host: "api.example.com"}}
	if err := c.CheckRedirect(next, []*http.Request{first}); err == nil {
		t.Fatal("same-host https->http redirect must be refused")
	}
	same := &http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com", Path: "/v2"}}
	if err := c.CheckRedirect(same, []*http.Request{first}); err != nil {
		t.Fatalf("same-origin redirect must pass: %v", err)
	}
}

func TestReadOnlyGitToolsRefusesDelimiterPaths(t *testing.T) {
	if got := readOnlyGitTools("/tmp/a,b"); got != nil {
		t.Fatalf("comma in path must withhold git tools, got %v", got)
	}
	if got := readOnlyGitTools("/tmp/a)b"); got != nil {
		t.Fatalf("paren in path must withhold git tools, got %v", got)
	}
	if got := readOnlyGitTools("/tmp/plain path"); len(got) == 0 {
		t.Fatal("ordinary path keeps the read-only grants")
	}
}

func TestCollectOneResearchFileSkipsSymlink(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(victim, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	research := t.TempDir()
	link := filepath.Join(research, "note.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	dest := t.TempDir()
	s := &SyncCmd{}
	if out := s.collectOneResearchFile(researchFile{path: link, rel: "note.md"}, dest, "personal", "proj"); out != collectFailed {
		t.Fatalf("symlinked research file must not be collected, got %v", out)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatalf("nothing may be written for a symlinked source, got %d entries", len(entries))
	}
}
