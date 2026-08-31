package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// qmd is distributed as an npm package, which puts it wherever the user's
// node version manager keeps its shims — for nvm that is
// ~/.nvm/versions/node/<version>/bin, a directory added to PATH by
// ~/.zshrc. A LaunchAgent runs `/bin/zsh -lc`, which is NOT interactive
// and therefore never sources ~/.zshrc, so `qmd` was simply absent from
// PATH for every scheduled run: every cron reindex was a silent no-op
// while `qmd` resolved fine in an interactive shell. `claude` escaped
// this only because it installs into ~/.local/bin, which path_helper
// puts on the default PATH.
//
// Two things are therefore required to invoke qmd from cron, and an
// absolute path alone is not enough:
//
//  1. locate the binary without relying on PATH (resolveQMDBinary), and
//  2. put its directory on the child's PATH (runQMD), because qmd is a
//     `#!/usr/bin/env node` script — without `node` resolvable the exec
//     fails with `env: node: No such file or directory`. node ships in
//     the same bin directory under every node version manager, so one
//     PATH entry satisfies both lookups.
//
// nodeManagerQMDGlobs are the per-user install roots to probe, in
// preference order. Kept to the managers that actually colocate node and
// the package shim; a manager that does not is a bug report, not a guess.
func nodeManagerQMDGlobs(home string) []string {
	return []string{
		filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "qmd"),
		filepath.Join(home, ".local", "share", "fnm", "node-versions", "*", "installation", "bin", "qmd"),
		filepath.Join(home, ".volta", "bin", "qmd"),
		filepath.Join(home, ".bun", "bin", "qmd"),
		filepath.Join(home, ".asdf", "installs", "nodejs", "*", "bin", "qmd"),
	}
}

// fixedQMDPaths are non-manager locations, probed after the managers.
func fixedQMDPaths(home string) []string {
	return []string{
		filepath.Join(home, ".local", "bin", "qmd"),
		"/opt/homebrew/bin/qmd",
		"/usr/local/bin/qmd",
		"/usr/bin/qmd",
	}
}

// resolveQMDBinary returns the qmd binary to exec. Order:
//
//  1. cfg.QMDPath   — explicit escape hatch for an unusual install
//  2. PATH          — correct for an interactive run, and for cron when
//     qmd lives somewhere path_helper already covers
//  3. probe         — the node-manager and fixed locations above
//
// Falls back to the bare name "qmd" when nothing is found, so the caller
// produces the same "executable file not found" error it always did
// rather than a confusing empty-path exec failure.
func resolveQMDBinary(root string) string {
	var explicit string
	if cfg := loadConfig(root); cfg != nil {
		explicit = cfg.QMDPath
	}
	return resolveQMDBinaryWith(explicit)
}

// resolveQMDBinaryWith is resolveQMDBinary for callers that already hold
// the config (doctor), so probing does not re-read scribe.yaml.
func resolveQMDBinaryWith(explicit string) string {
	if explicit != "" {
		if p := expandHome(explicit); isExecutableFile(p) {
			return p
		}
	}
	if p, err := exec.LookPath("qmd"); err == nil {
		return p
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "qmd"
	}
	for _, pattern := range nodeManagerQMDGlobs(home) {
		if p := newestExecutableMatch(pattern); p != "" {
			return p
		}
	}
	for _, p := range fixedQMDPaths(home) {
		if isExecutableFile(p) {
			return p
		}
	}
	return "qmd"
}

// newestExecutableMatch expands a glob and returns the executable match
// with the highest embedded version number. A machine that has held
// several node releases keeps all of them under nvm, and only the
// version qmd was `npm i -g`'d into has the shim — filtering on
// executability usually leaves exactly one. When it leaves several,
// newest wins, which matches what an interactive shell would have used.
func newestExecutableMatch(pattern string) string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	var found []string
	for _, m := range matches {
		if isExecutableFile(m) {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Slice(found, func(i, j int) bool {
		return compareVersionPaths(found[i], found[j]) > 0
	})
	return found[0]
}

// compareVersionPaths orders two paths by the first dotted numeric run in
// each, so v26.4.0 sorts above v9.0.0 (a plain string sort gets this
// backwards). Returns >0 when a is newer.
func compareVersionPaths(a, b string) int {
	av, bv := versionNumbers(a), versionNumbers(b)
	for i := 0; i < len(av) || i < len(bv); i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x - y
		}
	}
	return strings.Compare(a, b)
}

// versionNumbers pulls the longest dotted numeric sequence out of a path
// (".../node/v26.4.0/bin/qmd" -> [26 4 0]).
func versionNumbers(path string) []int {
	var best []int
	for _, seg := range strings.Split(path, string(os.PathSeparator)) {
		seg = strings.TrimPrefix(seg, "v")
		parts := strings.Split(seg, ".")
		nums := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				nums = nil
				break
			}
			nums = append(nums, n)
		}
		if len(nums) > len(best) {
			best = nums
		}
	}
	return best
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// runQMD execs qmd with its own directory prepended to PATH so the
// `#!/usr/bin/env node` shebang resolves. Returns trimmed combined
// output plus the error, matching runCmdErr's contract.
func runQMD(root string, args ...string) (string, error) {
	bin := resolveQMDBinary(root)
	cmd := exec.Command(bin, args...) //nolint:noctx // sync wrapper, mirrors runCmdErr
	if root != "" {
		cmd.Dir = root
	}
	cmd.Env = qmdEnv(bin, os.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// qmdEnv returns env with the binary's directory prepended to PATH. Only
// applied for an absolute resolution: prepending filepath.Dir("qmd")
// would put "." on PATH, which is both useless and a footgun.
func qmdEnv(bin string, env []string) []string {
	if !filepath.IsAbs(bin) {
		return env
	}
	dir := filepath.Dir(bin)
	out := make([]string, 0, len(env))
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+dir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+dir)
	}
	return out
}
