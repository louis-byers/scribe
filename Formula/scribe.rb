# Homebrew formula for scribe.
#
# Lives in a tap (e.g. `oliver-kriska/scribe`). Publish with:
#   brew tap-new <user>/scribe
#   cp Formula/scribe.rb $(brew --repo <user>/scribe)/Formula/
#   brew install <user>/scribe/scribe
#
# The URL + SHA256 placeholders are refreshed by goreleaser on every tagged
# release (see .goreleaser.yml → brews:).
class Scribe < Formula
  desc     "LLM-managed personal knowledge base tooling"
  homepage "https://github.com/oliver-kriska/scribe"
  version  "0.0.0"
  license  "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_ME_DARWIN_ARM64"
    else
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_ME_DARWIN_AMD64"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_ME_LINUX_ARM64"
    else
      url "https://github.com/oliver-kriska/scribe/releases/download/v#{version}/scribe_#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_ME_LINUX_AMD64"
    end
  end

  depends_on "git"
  depends_on "sqlite"

  # ccrider is the Claude-session recorder scribe reads via FTS5. It ships
  # from neilberkman's tap; brew auto-taps on install.
  depends_on "neilberkman/tap/ccrider"

  # Not declared here (no brew formula exists): `claude` (install via
  # `curl -fsSL https://claude.ai/install.sh | bash` or npm), `qmd`
  # (semantic-search over the KB, install separately), `trafilatura`
  # (optional, pip/pipx), `jq` and `fzf` (optional).

  def install
    bin.install "scribe"
  end

  def caveats
    <<~EOS
      Runtime dependencies not on Homebrew — install these separately:
        * claude     (Claude Code CLI)
                     curl -fsSL https://claude.ai/install.sh | bash
        * qmd        (semantic search over the KB)
                     npm install -g @tobilu/qmd
        * trafilatura (optional, URL → markdown)
                     pipx install trafilatura
        * jq, fzf    (optional)
                     brew install jq fzf

      Already installed by brew as dependencies: git, sqlite, ccrider.

      After installing:
        scribe init --path ~/my-kb --bind
        cd ~/my-kb && scribe skill install
        scribe cron install           # macOS: LaunchAgents
                                      # Linux: prints crontab lines to paste
        scribe doctor

      Personal, Ollama, hosted, and team recipes: https://getscribe.dev/setup.md

      macOS — Full Disk Access for `scribe capture` (iMessage):
        scribe fda                    # opens the FDA pane and walks you through
                                      # use drag-and-drop from Finder if the
                                      # "+ / Cmd-Shift-G" flow fails to register

      macOS release binaries are Developer ID signed, so in-place replacements
      keep a stable code identity. Homebrew also changes the raw executable's
      versioned Cellar path, which TCC records separately; after
      `brew upgrade scribe`, run `scribe fda` to verify and re-grant if needed.

      Scheduled jobs update themselves: the first one to run after an
      upgrade rewrites any LaunchAgent the new version changed. Plists you
      edited by hand are left alone; adopt them back with
      `scribe cron install --force`.
    EOS
  end

  # No post_install: Homebrew deprecates it, and its sandboxed HOME hides the
  # user's LaunchAgents anyway. Scheduled jobs refresh them after an upgrade
  # (cmd/scribe/agent_refresh.go), and caveats print on upgrade too.

  test do
    assert_match "scribe", shell_output("#{bin}/scribe --help 2>&1")
  end
end
