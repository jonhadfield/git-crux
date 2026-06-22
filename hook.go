package main

import (
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

// runInit installs the prepare-commit-msg hook into the current repository.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	out, err := run("git", "rev-parse", "--git-path", "hooks")
	if err != nil {
		return fmt.Errorf("not inside a git repository: %w", err)
	}
	hooksDir := strings.TrimSpace(out)
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")

	// Preserve an existing third-party hook so the shim can chain to it.
	if data, err := os.ReadFile(hookPath); err == nil {
		if strings.Contains(string(data), "git-crux") {
			fmt.Println("git-crux hook already installed at", hookPath)
			return nil
		}
		backup := hookPath + ".local"
		if err := os.Rename(hookPath, backup); err != nil {
			return fmt.Errorf("backing up existing hook: %w", err)
		}
		_ = os.Chmod(backup, 0o755)
		fmt.Println("preserved existing hook at", backup)
	}

	if err := os.WriteFile(hookPath, []byte(hookShim), 0o755); err != nil {
		return err
	}
	fmt.Println("installed git-crux hook at", hookPath)
	fmt.Println(`git commit -m "..." will now be checked. Bypass with GIT_CRUX_SKIP=1 or git commit --no-verify.`)
	return nil
}

// runHook is the prepare-commit-msg entry point. Args are the hook name followed
// by git's positional arguments: <msgfile> [source] [sha]. It must FAIL OPEN —
// any reason to back out returns nil so the commit proceeds untouched.
func runHook(args []string) error {
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
		v, err := evaluate("", diff, modelName())
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
	v, err := evaluate(original, diff, modelName())
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
