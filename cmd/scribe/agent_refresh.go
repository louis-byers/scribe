package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Upgrade self-heal for the macOS LaunchAgents.
//
// A release can add, drop or reschedule a job, but launchd keeps running
// the plists the previous binary wrote until something rewrites and
// reloads them. The Homebrew formula's post_install used to run
// `cron install --if-installed` for that, and it never worked on a current
// Homebrew: since 5.1.15 post-install runs in a write sandbox with HOME
// pointed at a temp dir, so the command saw no agents and reported
// "nothing to refresh". Homebrew 7 then deprecated post_install, and its
// replacement (post_install_steps) runs commands in that same sandbox.
//
// So the binary heals itself. A scheduled job is the one thing guaranteed
// to run in the user's real launchd session, with the real HOME, so the
// first job a new version runs refreshes whatever plists are stale — which
// also covers `make install` and any other way a new binary lands.
//
// The trap: a reload is bootout + bootstrap, and bootout kills what the job
// is running, including the process doing the refresh. So a refresh never
// reloads its own job, and never reloads another scheduled job that is
// running right now (that would kill its sync or dream mid-run). Those wait
// for the next job to fire, and the version is recorded only once a pass
// holds nothing back.

// agentRefreshLockTTL is how old a refresh lock may get before it is
// treated as abandoned. A pass is a few launchctl calls; ten minutes means
// the process died holding it.
const agentRefreshLockTTL = 10 * time.Minute

// agentsVersionPath records the scribe version that last finished checking
// the LaunchAgents, so each version checks them once.
func agentsVersionPath() string { return filepath.Join(userConfigDir(), "agents-version") }

func agentRefreshLockPath() string { return filepath.Join(userConfigDir(), "agents-refresh.lock") }

// maybeRefreshAgents runs before a command when launchd started it. cmdPath
// is kong's command path ("each <args>", "watch"). It never fails the
// command: a refresh problem is reported and the job runs anyway.
func maybeRefreshAgents(cmdPath string) {
	if runtime.GOOS != "darwin" {
		return
	}
	self := os.Getenv("XPC_SERVICE_NAME")
	if !strings.HasPrefix(self, "com.scribe.") {
		return
	}
	// Only the commands the agents themselves run. Anything they spawn
	// (each's per-KB children, a `claude -p` whose hooks call scribe)
	// inherits XPC_SERVICE_NAME, and must not start a second refresh.
	if cmd, _, _ := strings.Cut(cmdPath, " "); cmd != "each" && cmd != "watch" {
		return
	}
	refreshAgents(self, version)
}

// refreshAgents brings the LaunchAgents up to date for binary version ver,
// at most once per version. self is the label of the running job.
func refreshAgents(self, ver string) {
	// A `go run` or test binary has no version worth recording.
	if ver == "" || ver == "dev" {
		return
	}
	stampPath := agentsVersionPath()
	if agentsVersion(stampPath) == ver {
		return
	}
	unlock, ok := lockAgentRefresh(agentRefreshLockPath(), time.Now())
	if !ok {
		return // another job is refreshing right now
	}
	defer unlock()
	prev := agentsVersion(stampPath) // re-read: the lock holder before us may have finished
	if prev == ver {
		return
	}
	if prev == "" {
		prev = "an earlier version"
	}
	// The plists name whatever `cron install` would resolve. If that is not
	// the binary running this job (a stray ~/.local/bin/scribe beside a brew
	// install, a PATH without the brew prefix), a refresh would silently
	// move the whole schedule onto it. That choice is the user's.
	binary := resolveScribeBinary()
	if !sameExecutable(binary) {
		fmt.Fprintf(os.Stderr, "scribe %s: not refreshing LaunchAgents: they would run %s, which is not this binary — run `scribe cron install` to choose\n", ver, binary)
		_ = writeGlobalState("", false, stampPath, []byte(ver+"\n"), 0o644)
		return
	}
	fmt.Printf("scribe %s: refreshing LaunchAgents (last checked by %s)\n", ver, prev)

	// Legacy single-KB plists (pre-#26) serve the KB they cd into. Keep
	// that KB scheduled before they are replaced, as `cron install` does.
	if other := otherKBServedByAgents(""); other != "" {
		if _, err := registerKB(other); err != nil {
			fmt.Fprintf(os.Stderr, "scribe: LaunchAgent refresh: could not register %s: %v\n", other, err)
			return
		}
	}

	domain := guiDomain()
	hold := func(job cronJob) bool {
		label := plistLabel(job.Name)
		if label == self {
			return true
		}
		// A KeepAlive watcher holds no work, and it is always running,
		// so holding it would never let it pick up a new definition.
		// Restart it.
		if job.KeepAlive {
			return false
		}
		return agentBusy(domain, label)
	}
	held, err := installAgents("", scribeJobs(binary), false, hold)
	if err != nil {
		// Not a transient failure (launchctl errors are only printed), so
		// retrying on every tick would just repeat it. Record the version
		// and say what to run.
		fmt.Fprintf(os.Stderr, "scribe: LaunchAgent refresh failed: %v — run `scribe cron install`\n", err)
	} else if len(held) > 0 {
		fmt.Printf("  %s left for the next scheduled run (running now)\n", strings.Join(held, ", "))
		return
	}
	if err := writeGlobalState("", false, stampPath, []byte(ver+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "scribe: %v\n", err)
	}
}

// selfExecutable is os.Executable, indirected for tests.
var selfExecutable = os.Executable

// sameExecutable reports whether path is the file this process runs,
// through any symlinks (brew's bin/scribe points into the Cellar).
func sameExecutable(path string) bool {
	exe, err := selfExecutable()
	if err != nil {
		return false
	}
	a, errA := os.Stat(path)
	b, errB := os.Stat(exe)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

func agentsVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// lockAgentRefresh takes an exclusive lock file, breaking one older than
// agentRefreshLockTTL. Two jobs can fire in the same minute; without the
// lock both would reload the same idle agents at once.
func lockAgentRefresh(path string, now time.Time) (unlock func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false
		}
		fi, statErr := os.Stat(path)
		if statErr != nil || now.Sub(fi.ModTime()) < agentRefreshLockTTL {
			return nil, false
		}
		_ = os.Remove(path) // abandoned: its holder died mid-pass
	}
	return nil, false
}

// agentIdleRe matches the top-level state line `launchctl print` shows for
// a loaded job that is not running. Nested blocks are indented deeper.
var agentIdleRe = regexp.MustCompile(`(?m)^\tstate = not running$`)

// agentBusy reports whether reloading label now could interrupt it. Only a
// job launchd positively reports as not running counts as idle: holding a
// job costs one scheduling tick, killing one loses its run.
func agentBusy(domain, label string) bool {
	out, err := runLaunchctl("print", domain+"/"+label)
	if err != nil || strings.Contains(strings.ToLower(out), "could not find") {
		return false // not loaded, so there is nothing to interrupt
	}
	return !agentIdleRe.MatchString(out)
}
