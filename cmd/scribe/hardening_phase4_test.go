package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 4A of the 2026-10-15 audit plan: symlinks on push-reachable
// paths, predictable temp names, unbounded decompression and downloads.

func TestWriteFileAtomicIgnoresPlantedTmpSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "wiki", "page.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// The old fixed temp name: a committed symlink here used to be
	// followed by os.WriteFile and truncate the victim.
	if err := os.Symlink(outside, target+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("new page"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "precious" {
		t.Fatalf("file outside the KB was written through the planted symlink: %q", got)
	}
	if got, _ := os.ReadFile(target); string(got) != "new page" {
		t.Fatalf("target not written: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scribe-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestReadRegularFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "real.md")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(regular); err != nil {
		t.Fatalf("regular file refused: %v", err)
	}
	if _, err := readRegularFile(link); err == nil {
		t.Fatal("symlink was read")
	}
}

func TestWalkArticlesSkipsSymlinkedFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("---\ntitle: Secret\n---\nleak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "ok.md"), []byte("---\ntitle: OK\n---\nfine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "wiki", "leak.md")); err != nil {
		t.Fatal(err)
	}
	var seen []string
	_ = walkArticles(root, func(path string, _ []byte) error {
		seen = append(seen, filepath.Base(path))
		return nil
	})
	if strings.Join(seen, ",") != "ok.md" {
		t.Fatalf("walked %v, want only ok.md", seen)
	}
}

func TestInboxSkipsSymlinks(t *testing.T) {
	root := testKB(t, "")
	outside := filepath.Join(t.TempDir(), "ssh_config.txt")
	if err := os.WriteFile(outside, []byte("Host prod\n  IdentityFile ~/.ssh/id_prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(root, "raw", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inbox, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 0 {
		t.Fatalf("ingested %d file(s) from a symlink", n)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "raw", "articles", "*"))
	for _, m := range matches {
		data, _ := os.ReadFile(m)
		if strings.Contains(string(data), "IdentityFile") {
			t.Fatalf("symlink target leaked into %s", m)
		}
	}
}

func buildZip(t *testing.T, name string, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("a"), size)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadZipEntryIsBounded(t *testing.T) {
	old := maxZipEntryBytes
	maxZipEntryBytes = 1024
	t.Cleanup(func() { maxZipEntryBytes = old })

	data := buildZip(t, "word/document.xml", 4096)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readZipEntry(zr.File[0]); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized entry accepted: %v", err)
	}
	if _, err := convertDOCXTier0(data); err == nil {
		t.Fatal("docx converter accepted an oversized document.xml")
	}
	small := buildZip(t, "word/document.xml", 100)
	zr, _ = zip.NewReader(bytes.NewReader(small), int64(len(small)))
	if got, err := readZipEntry(zr.File[0]); err != nil || len(got) != 100 {
		t.Fatalf("small entry: %v (%d bytes)", err, len(got))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func TestCopyBounded(t *testing.T) {
	if _, err := copyBounded(io.Discard, io.LimitReader(zeroReader{}, 1024)); err != nil {
		t.Fatalf("small copy failed: %v", err)
	}
	if _, err := copyBounded(io.Discard, io.LimitReader(zeroReader{}, maxHTTPBodyBytes+4096)); err == nil {
		t.Fatal("oversized body copied without error")
	}
}

// --- Phase 4B: symlink escapes through directories ------------------------

func TestValidateActionPathRefusesSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	// wiki/sub -> /outside : lexically fine, physically an escape.
	if err := os.Symlink(outside, filepath.Join(root, "wiki", "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := validateActionPath(root, "wiki/sub/page.md"); err == nil {
		t.Fatal("write through a symlinked directory accepted")
	}
	if _, err := validateActionPath(root, "wiki/sub/deeper/new/page.md"); err == nil {
		t.Fatal("write below a symlinked directory accepted")
	}
	if _, err := validateActionPath(root, "wiki/inside/page.md"); err != nil {
		t.Fatalf("plain path refused: %v", err)
	}
	if _, err := validateActionPath(root, "wiki/brand/new/dirs/page.md"); err != nil {
		t.Fatalf("not-yet-existing path refused: %v", err)
	}
	// A symlinked FILE whose target is outside.
	victim := filepath.Join(outside, "victim.md")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "wiki", "alias.md")); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{Op: "append", Path: "wiki/alias.md", Content: "\npwned\n"}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("append went through a symlinked file: %q", got)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("expected one refusal, got %v", res.Errors)
	}
	// A symlink that stays inside the KB is still allowed.
	if err := os.Symlink(filepath.Join(root, "wiki", "inside"), filepath.Join(root, "wiki", "local-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := validateActionPath(root, "wiki/local-link/page.md"); err != nil {
		t.Fatalf("in-KB symlink refused: %v", err)
	}
}

func TestRelationsRevertKeepsPathsInsideKB(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)
	outside := filepath.Join(t.TempDir(), "victim.md")
	if err := os.WriteFile(outside, []byte("---\ntitle: V\nrelated: [\"[[X]]\"]\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(root, outside)
	log := `{"source":"` + rel + `","target":"X","to_field":"depends_on","reverse":""}` + "\n"
	logPath := filepath.Join(root, "wiki", "_relations_migration_test.jsonl")
	if err := os.WriteFile(logPath, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(outside)
	_ = (&RelationsMigrateRevertCmd{Log: logPath}).Run()
	after, _ := os.ReadFile(outside)
	if !bytes.Equal(before, after) {
		t.Fatalf("revert edited a file outside the KB")
	}
}

// --- Phase 6: defense in depth ------------------------------------------

func TestFetchURLRefusesNonPublicTargets(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x",
		"http://localhost:8080/admin",
		"http://127.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://[::1]/",
	} {
		if _, err := fetchURL(ctx, raw, ""); err == nil {
			t.Errorf("%s: expected refusal", raw)
		} else if strings.Contains(err.Error(), "trafilatura") || strings.Contains(err.Error(), "defuddle") {
			t.Errorf("%s: reached a fetcher before the policy check: %v", raw, err)
		}
	}
}

func TestRequireSecureBaseURL(t *testing.T) {
	for _, ok := range []string{"https://api.together.xyz/v1", "http://localhost:1234/v1", "http://127.0.0.1:8000/v1", "http://[::1]:8000/v1"} {
		if err := requireSecureBaseURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://api.example.com/v1", "http://10.0.0.5/v1", "", "not a url"} {
		if err := requireSecureBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
