package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hookShim is written to .git/hooks/prepare-commit-msg. It chains to any
// pre-existing hook (preserved as prepare-commit-msg.local) so git-crux does not
// clobber other tools, then hands control to the binary.
const hookShim = `#!/bin/sh
# Installed by git-crux. Do not edit; re-run "git crux init" to refresh.
local_hook="$(dirname "$0")/prepare-commit-msg.local"
[ -x "$local_hook" ] && "$local_hook" "$@"
exec git-crux hook prepare-commit-msg "$@"
`

// runInit installs the prepare-commit-msg hook. By default it installs into the
// current repository; with --global it installs once for every repo via
// core.hooksPath.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	global := fs.Bool("global", false, "install once for all repos via core.hooksPath")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *global {
		return initGlobal()
	}

	out, err := run("git", "rev-parse", "--git-path", "hooks")
	if err != nil {
		return fmt.Errorf("not inside a git repository: %w", err)
	}
	hookPath, fresh, err := installHookInto(strings.TrimSpace(out))
	if err != nil {
		return err
	}
	if !fresh {
		fmt.Println("git-crux hook already installed at", hookPath)
		return nil
	}
	fmt.Println("installed git-crux hook at", hookPath)
	fmt.Println(`git commit -m "..." will now be checked. Bypass with GIT_CRUX_SKIP=1 or git commit --no-verify.`)
	return nil
}

// initGlobal installs the hook into a shared directory and points git's global
// core.hooksPath at it, so every repository is covered without per-repo setup.
// An already-configured core.hooksPath is reused; otherwise it defaults to
// ~/.config/git/hooks.
func initGlobal() error {
	hooksDir := strings.TrimSpace(gitConfigGlobal("core.hooksPath"))
	if hooksDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		hooksDir = filepath.Join(home, ".config", "git", "hooks")
	}
	hooksDir = expandTilde(hooksDir)

	hookPath, fresh, err := installHookInto(hooksDir)
	if err != nil {
		return err
	}
	if _, err := run("git", "config", "--global", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("setting global core.hooksPath: %w", err)
	}

	if fresh {
		fmt.Println("installed git-crux hook at", hookPath)
	} else {
		fmt.Println("git-crux hook already installed at", hookPath)
	}
	fmt.Println("set global core.hooksPath to", hooksDir)
	fmt.Println("note: core.hooksPath replaces per-repo .git/hooks; existing repo hooks won't run unless moved here.")
	return nil
}

// installHookInto writes the hook shim into hooksDir, preserving any pre-existing
// third-party hook as prepare-commit-msg.local so the shim can chain to it. fresh
// is false when a git-crux hook is already present (nothing was rewritten).
func installHookInto(hooksDir string) (hookPath string, fresh bool, err error) {
	if err = os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", false, err
	}
	hookPath = filepath.Join(hooksDir, "prepare-commit-msg")

	if data, e := os.ReadFile(hookPath); e == nil {
		if strings.Contains(string(data), "git-crux") {
			return hookPath, false, nil
		}
		backup := hookPath + ".local"
		if e := os.Rename(hookPath, backup); e != nil {
			return "", false, fmt.Errorf("backing up existing hook: %w", e)
		}
		_ = os.Chmod(backup, 0o755)
		fmt.Println("preserved existing hook at", backup)
	}

	if err = os.WriteFile(hookPath, []byte(hookShim), 0o755); err != nil {
		return "", false, err
	}
	return hookPath, true, nil
}

// gitConfigGlobal returns a global git config value, or "" if unset.
func gitConfigGlobal(key string) string {
	out, err := run("git", "config", "--global", "--get", key)
	if err != nil {
		return ""
	}
	return out
}

// expandTilde resolves a leading ~ in a path to the user's home directory.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// runHook is the prepare-commit-msg entry point. Args are the hook name followed
// by git's positional arguments: <msgfile> [source] [sha]. It must FAIL OPEN —
// any reason to back out returns nil so the commit proceeds untouched.
func runHook(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "prepare-commit-msg" {
		return nil // not a hook we handle
	}
	msgFile := args[1]
	source := ""
	if len(args) >= 3 {
		source = args[2]
	}

	// Skip contexts where rewriting the message is unwanted or unsafe.
	switch source {
	case "merge", "squash", "commit": // merge/squash/amend/-c reuse
		return nil
	}
	if os.Getenv("GIT_CRUX_SKIP") != "" {
		return nil
	}
	if !isInteractive() {
		return nil
	}

	raw, err := os.ReadFile(msgFile)
	if err != nil {
		return err
	}
	original := stripComments(string(raw))

	diff, err := stagedDiff()
	if err != nil || strings.TrimSpace(diff) == "" {
		return nil // nothing to work with; fail open
	}

	// Empty message on a plain `git commit` (source is empty — no -m, no
	// template): generate a message and drop it into the editor buffer. The
	// editor itself is the review surface, so we do not prompt; the developer
	// edits and saves to accept, or empties the buffer to abort.
	if original == "" {
		if source != "" {
			return nil // a template or other prefilled source; leave it alone
		}
		v, err := evaluate(ctx, "", diff, modelName())
		if err != nil {
			fmt.Fprintln(os.Stderr, "git-crux:", err)
			return nil
		}
		if strings.TrimSpace(v.Suggestion) == "" {
			return nil
		}
		return writeMessage(msgFile, v.Suggestion, string(raw))
	}

	// Non-empty message: judge it and, if off-point, offer a sharper one.
	v, err := evaluate(ctx, original, diff, modelName())
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-crux:", err)
		return nil // model unreachable; never block the commit
	}
	if v.Verdict == "accurate" || v.Suggestion == "" {
		return nil
	}

	final := promptUser(original, v)
	if final == original {
		return nil
	}
	return writeMessage(msgFile, final, string(raw))
}

// writeMessage replaces the human message while preserving any trailing git
// comment/template lines from the original file.
func writeMessage(path, message, raw string) error {
	var b strings.Builder
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteString("\n")
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "#") {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
