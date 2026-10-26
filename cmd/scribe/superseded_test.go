package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const supersededStubArticle = "---\ntitle: \"Old Approach\"\ntype: pattern\ndomain: general\nstatus: superseded\n---\n\nSuperseded. See [[New Approach]].\n"

// supersededTestKB builds a KB with:
//
//	wiki/new.md   "New Approach" (live)  → links to [[Old Approach]]
//	wiki/other.md "Other Note"   (live)  → links to [[New Approach]]
//	wiki/old.md   "Old Approach" (status: superseded) → links to [[New Approach]]
func supersededTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "wiki/new.md",
		"---\ntitle: \"New Approach\"\ntype: pattern\ndomain: general\n---\n\nThe current way. Replaces [[Old Approach]].\n")
	writeKBFile(t, root, "wiki/other.md",
		"---\ntitle: \"Other Note\"\ntype: solution\ndomain: general\n---\n\nUses [[New Approach]].\n")
	writeKBFile(t, root, "wiki/old.md", supersededStubArticle)
	return root
}

func TestIsSuperseded(t *testing.T) {
	cases := []struct {
		name string
		fm   *Frontmatter
		want bool
	}{
		{"nil", nil, false},
		{"no status", &Frontmatter{}, false},
		{"superseded", &Frontmatter{Status: "superseded"}, true},
		{"other status", &Frontmatter{Status: "decided"}, false},
		{"case differs", &Frontmatter{Status: "Superseded"}, false},
	}
	for _, c := range cases {
		if got := isSuperseded(c.fm); got != c.want {
			t.Errorf("%s: isSuperseded = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIndexOmitsSupersededArticles(t *testing.T) {
	root := supersededTestKB(t)
	var err error
	captureLintStdout(t, func() { err = (&IndexCmd{}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "- [[Old Approach]]") {
		t.Errorf("superseded article listed in the index:\n%s", content)
	}
	if !strings.Contains(content, "- [[New Approach]]") {
		t.Errorf("live article missing from the index:\n%s", content)
	}
	if !strings.Contains(content, "article_count: 2") {
		t.Errorf("article_count should count only live articles (2):\n%s", content)
	}
}

// Superseded articles drop out of the index, so the consistency lint must
// not count them on disk either, or a KB with 4+ of them warns about drift
// that isn't there.
func TestLintIndexConsistencyIgnoresSupersededArticles(t *testing.T) {
	root := supersededTestKB(t)
	for i, title := range []string{"Old B", "Old C", "Old D"} {
		writeKBFile(t, root, filepath.Join("wiki", "old-"+string(rune('b'+i))+".md"),
			strings.Replace(supersededStubArticle, "Old Approach", title, 1))
	}
	writeKBFile(t, root, "wiki/_index.md", "- [[New Approach]] -- s\n- [[Other Note]] -- s\n")
	var warnings int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintIndexConsistency(rep, root)
		warnings = rep.warnings
	})
	if warnings != 0 {
		t.Errorf("warnings = %d, want 0 with 4 superseded articles on disk\noutput:\n%s", warnings, out)
	}
	if !strings.Contains(out, "index: 2 entries, disk: 2 articles") {
		t.Errorf("disk count should exclude superseded articles:\n%s", out)
	}
}
