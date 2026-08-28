package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeQMD puts an executable `qmd` on PATH that exits with the given
// code after printing msg, so reindexQMD can be exercised without the
// real binary or the developer's global qmd store.
func fakeQMD(t *testing.T, exitCode int, msg string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"" + msg + "\"\nexit " + strconv.Itoa(exitCode) + "\n"
	path := filepath.Join(dir, "qmd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qmd: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A failing qmd used to be invisible: every call site went through
// runCmd, which returns "" and drops the error, so sync logged "qmd
// reindex complete" over a reindex that never happened. reindexQMD must
// surface it instead.
func TestReindexQMD_SurfacesFailure(t *testing.T) {
	t.Setenv("SCRIBE_SKIP_REINDEX", "")
	fakeQMD(t, 1, "boom: index locked")

	err := reindexQMD(t.TempDir(), "test")
	if err == nil {
		t.Fatal("reindexQMD returned nil on a qmd that exited 1 — failure is still being swallowed")
	}
	if !strings.Contains(err.Error(), "boom: index locked") {
		t.Errorf("error %q does not carry qmd's output", err)
	}
	// Both steps are attempted and both reported, so a partial reindex
	// (update ok, embed broken) is not mistaken for a clean one.
	if !strings.Contains(err.Error(), "qmd update") || !strings.Contains(err.Error(), "qmd embed") {
		t.Errorf("error %q should name both failed steps", err)
	}
}

func TestReindexQMD_NilOnSuccess(t *testing.T) {
	t.Setenv("SCRIBE_SKIP_REINDEX", "")
	fakeQMD(t, 0, "ok")

	if err := reindexQMD(t.TempDir(), "test"); err != nil {
		t.Errorf("reindexQMD = %v, want nil when qmd succeeds", err)
	}
}

// The guard exists so the suite never touches the real qmd index.
func TestReindexQMD_HonorsSkipEnv(t *testing.T) {
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")
	fakeQMD(t, 1, "should not run")

	if err := reindexQMD(t.TempDir(), "test"); err != nil {
		t.Errorf("reindexQMD = %v, want nil when SCRIBE_SKIP_REINDEX=1", err)
	}
}
