package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// refreshRig is a sandboxed machine for the upgrade self-heal: a fake HOME,
// a fake scribe binary that is both "on PATH" and "this process", and a
// launchctl stub that reports each label's state and records every call.
// Nothing here reaches the real launchd.
type refreshRig struct {
	t      *testing.T
	home   string
	binary string
	state  map[string]string // label → "running" | "idle"; absent = not loaded
	calls  [][]string
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	r := &refreshRig{t: t, home: t.TempDir(), state: map[string]string{}}
	t.Setenv("HOME", r.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	binDir := t.TempDir()
	r.binary = filepath.Join(binDir, "scribe")
	if err := os.WriteFile(r.binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No ~/.local/bin/scribe under the fake HOME, so resolveScribeBinary
	// falls through to PATH and finds r.binary.
	t.Setenv("PATH", binDir)

	origRun, origDelay, origExe := runLaunchctl, bootstrapRetryDelay, selfExecutable
	t.Cleanup(func() { runLaunchctl, bootstrapRetryDelay, selfExecutable = origRun, origDelay, origExe })
	bootstrapRetryDelay = 0
	selfExecutable = func() (string, error) { return r.binary, nil }
	runLaunchctl = func(args ...string) (string, error) {
		r.calls = append(r.calls, args)
		if args[0] != "print" {
			return "", nil
		}
		label := args[1][strings.LastIndex(args[1], "/")+1:]
		switch r.state[label] {
		case "running":
			return label + " = {\n\tstate = running\n\tpid = 4242\n\tendpoints = {\n\t\tstate = not running\n\t}\n}\n", nil
		case "idle":
			return label + " = {\n\tstate = not running\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n", nil
		default:
			return "Bad request.\nCould not find service \"" + label + "\" in domain for user gui: 501\n", errors.New("exit status 113")
		}
	}
	if err := os.MkdirAll(filepath.Join(r.home, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// installStale writes, for every job, a plist scribe authored for an older
// binary: validly stamped, but not what this binary would write. All jobs
// start loaded and idle.
func (r *refreshRig) installStale() {
	r.t.Helper()
	for _, job := range scribeJobs(r.binary) {
		old := job
		old.Command = strings.Replace(job.Command, r.binary, "/old/bin/scribe", 1)
		r.write(job.Name, stampPlist(renderPlist(old)))
		r.state[plistLabel(job.Name)] = "idle"
	}
}

func (r *refreshRig) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(plistPath(name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *refreshRig) current(name string) bool {
	r.t.Helper()
	for _, job := range scribeJobs(r.binary) {
		if job.Name == name {
			got, err := os.ReadFile(plistPath(name))
			if err != nil {
				r.t.Fatal(err)
			}
			return string(got) == stampPlist(renderPlist(job))
		}
	}
	r.t.Fatalf("no job %q", name)
	return false
}

func (r *refreshRig) bootedOut(name string) bool {
	return slices.ContainsFunc(r.calls, func(c []string) bool {
		return c[0] == "bootout" && strings.HasSuffix(c[1], "/"+plistLabel(name))
	})
}

func (r *refreshRig) stamp() string { return agentsVersion(agentsVersionPath()) }

// TestRefreshAgentsNeverReloadsARunningJob is the property the whole design
// rests on: bootout kills what a job is running, so the refresh must not
// reload its own job or another scheduled job mid-run. Those wait for a
// later pass, and the version is recorded only when nothing was held.
func TestRefreshAgentsNeverReloadsARunningJob(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	r.state["com.scribe.ingest-drain"] = "running"  // the job doing the refresh
	r.state["com.scribe.sync-sessions"] = "running" // mid-run, not ours
	r.state["com.scribe.watch"] = "running"         // KeepAlive: always running
	unstamped := "<plist>hand-edited</plist>\n"
	r.write("lint-fix", unstamped)

	refreshAgents("com.scribe.ingest-drain", "0.9.0")

	for _, held := range []string{"ingest-drain", "sync-sessions"} {
		if r.current(held) {
			t.Errorf("%s was rewritten while running; it would then read as current and never be reloaded", held)
		}
		if r.bootedOut(held) {
			t.Errorf("%s was booted out while running — that kills its run", held)
		}
	}
	for _, reloaded := range []string{"lint", "watch"} {
		if !r.current(reloaded) || !r.bootedOut(reloaded) {
			t.Errorf("%s: want rewritten and reloaded (current=%v bootout=%v)", reloaded, r.current(reloaded), r.bootedOut(reloaded))
		}
	}
	if got := r.stamp(); got != "" {
		t.Errorf("stamp written while jobs were held back: %q", got)
	}

	// The next job to fire finishes the pass once both are idle again.
	r.state["com.scribe.ingest-drain"] = "idle"
	r.state["com.scribe.sync-sessions"] = "idle"
	r.state["com.scribe.lint"] = "running"
	r.calls = nil
	refreshAgents("com.scribe.lint", "0.9.0")
	for _, name := range []string{"ingest-drain", "sync-sessions"} {
		if !r.current(name) || !r.bootedOut(name) {
			t.Errorf("second pass: %s not refreshed", name)
		}
	}
	if r.bootedOut("lint") {
		t.Error("second pass reloaded its own (already current) job")
	}
	got, _ := os.ReadFile(plistPath("lint-fix"))
	if string(got) != unstamped {
		t.Error("a hand-edited plist was overwritten")
	}
	if got := r.stamp(); got != "0.9.0" {
		t.Errorf("stamp after a pass that held nothing = %q, want 0.9.0 (a hand-edited plist is skipped, not held)", got)
	}

	// Once recorded, a version never looks at launchd again.
	r.calls = nil
	refreshAgents("com.scribe.dream", "0.9.0")
	if len(r.calls) != 0 {
		t.Errorf("refresh ran again for a recorded version: %v", r.calls)
	}
	if _, err := os.Stat(agentRefreshLockPath()); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// TestRefreshAgentsFromTheWatcher: other KeepAlive jobs are restarted, but
// the watcher refreshing itself would boot itself out and never get to the
// bootstrap — leaving it unloaded until the next login.
func TestRefreshAgentsFromTheWatcher(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	r.state["com.scribe.watch"] = "running"

	refreshAgents("com.scribe.watch", "0.9.0")

	if r.bootedOut("watch") || r.current("watch") {
		t.Error("the watcher reloaded itself mid-refresh")
	}
	if !r.current("lint") {
		t.Error("the watcher's pass refreshed nothing else")
	}
	if r.stamp() != "" {
		t.Error("stamp written while the watcher itself was still stale")
	}
}

// TestRefreshAgentsLeavesTheScheduleOnItsBinary: when `cron install` would
// resolve a different binary than the one running the job, the refresh
// must not move every agent onto it.
func TestRefreshAgentsLeavesTheScheduleOnItsBinary(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	other := filepath.Join(t.TempDir(), "scribe")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfExecutable = func() (string, error) { return other, nil }

	refreshAgents("com.scribe.lint", "0.9.0")

	if len(r.calls) != 0 {
		t.Errorf("launchctl called for a foreign binary: %v", r.calls)
	}
	if r.current("lint") {
		t.Error("plists were re-pointed at a binary this job does not run")
	}
	if got := r.stamp(); got != "0.9.0" {
		t.Errorf("stamp = %q; the refusal should be reported once per version, not every tick", got)
	}
}

func TestRefreshAgentsSkipsUnversionedBuilds(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	for _, ver := range []string{"", "dev"} {
		refreshAgents("com.scribe.lint", ver)
	}
	if len(r.calls) != 0 || r.current("lint") || r.stamp() != "" {
		t.Errorf("an unversioned build touched the agents (calls=%v stamp=%q)", r.calls, r.stamp())
	}
}

func TestRefreshAgentsRespectsTheLock(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	lock := agentRefreshLockPath()
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	refreshAgents("com.scribe.dream", "0.9.0")
	if len(r.calls) != 0 || r.current("lint") {
		t.Fatalf("ran while another job held the lock (calls=%v)", r.calls)
	}

	// A lock its holder never released is broken after the TTL.
	old := time.Now().Add(-2 * agentRefreshLockTTL)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	refreshAgents("com.scribe.dream", "0.9.0")
	if !r.current("lint") {
		t.Error("an abandoned lock blocked the refresh forever")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// TestMaybeRefreshAgentsOnlyFromTheAgentsOwnCommands: children of a job
// inherit XPC_SERVICE_NAME, so the label alone is not enough.
func TestMaybeRefreshAgentsOnlyFromTheAgentsOwnCommands(t *testing.T) {
	r := newRefreshRig(t)
	r.installStale()
	origVersion := version
	t.Cleanup(func() { version = origVersion })
	version = "0.9.0"

	for _, tc := range []struct{ service, cmd string }{
		{"", "each <args>"},
		{"application.com.apple.Terminal.123", "each <args>"},
		{"com.scribe.sync-sessions", "sync"},
		{"com.scribe.sync-sessions", "hook"},
		{"com.scribe.sync-sessions", "eachx"},
	} {
		t.Setenv("XPC_SERVICE_NAME", tc.service)
		maybeRefreshAgents(tc.cmd)
		if len(r.calls) != 0 || r.stamp() != "" {
			t.Fatalf("service=%q cmd=%q started a refresh", tc.service, tc.cmd)
		}
	}

	if runtime.GOOS != "darwin" {
		return // the positive case is darwin-only by design
	}
	t.Setenv("XPC_SERVICE_NAME", "com.scribe.watch")
	maybeRefreshAgents("watch")
	if !r.current("lint") {
		t.Error("the watch agent's own command did not refresh")
	}
}

func TestAgentBusy(t *testing.T) {
	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"not loaded (exit 113)", "Could not find service", errors.New("exit status 113"), false},
		{"not loaded (exit 0)", "Could not find service \"x\"", nil, false},
		{"idle", "x = {\n\tstate = not running\n}\n", nil, false},
		{"running", "x = {\n\tstate = running\n\tpid = 1\n}\n", nil, true},
		{"only a nested block says not running", "x = {\n\tpid = 1\n\tm = {\n\t\tstate = not running\n\t}\n}\n", nil, true},
		{"unrecognized output", "x = {\n}\n", nil, true},
	} {
		runLaunchctl = func(...string) (string, error) { return tc.out, tc.err }
		if got := agentBusy("gui/501", "com.scribe.x"); got != tc.want {
			t.Errorf("%s: agentBusy = %v, want %v", tc.name, got, tc.want)
		}
	}
}
