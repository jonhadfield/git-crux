// Command git-crux checks a commit message against the actual staged diff and,
// when the message is off-point, suggests a sharper one.
//
// Two entry points share the same logic:
//
//	git crux -m "message"   explicit command: evaluate, then commit
//	git-crux hook ...        prepare-commit-msg hook: evaluate, rewrite message file
//
// Named git-crux so that git's subcommand dispatch makes `git crux` work once
// the binary is on PATH.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const version = "0.2.0"

func main() {
	// A signal-cancellable context so Ctrl-C aborts an in-flight model call
	// (which can wait up to 90s on a cold model load) instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		// Bare `git crux` means: generate a commit message from the staged diff
		// and commit. `git crux help` still shows usage.
		if err := runCommit(ctx, nil); err != nil {
			fail(err)
		}
		return
	}

	switch os.Args[1] {
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fail(err)
		}
	case "hook":
		// Hook path must FAIL OPEN: log to stderr but never block the commit.
		if err := runHook(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "git-crux:", err)
		}
		os.Exit(0)
	case "version", "--version", "-v":
		fmt.Println("git-crux", version)
	case "help", "--help", "-h":
		usage()
	default:
		// Anything else is treated as the commit path, e.g. `git crux -m "..."`.
		if err := runCommit(ctx, os.Args[1:]); err != nil {
			fail(err)
		}
	}
}

func usage() {
	fmt.Print(`git-crux ` + version + ` — keep commit messages on point.

Usage:
  git crux                  Generate a commit message from staged changes, then commit.
  git crux -m "message"     Evaluate your message against staged changes, then commit.
  git crux init             Install the prepare-commit-msg hook in this repo.
  git crux init --global    Install the hook for all repos via core.hooksPath.
  git crux version          Print the version.

Commit flags:
  -m string      Commit message. Omit to have git-crux generate one.
  -model string  Model to use (default: from GIT_CRUX_MODEL, the server profile, or "` + defaultModel + `").
  -style string  Message style: conventional|plain (default "` + defaultStyle + `").
  -no-ai         Skip evaluation and commit as-is.
  -dry-run       Print the verdict as JSON and exit without committing.

Environment:
  GIT_CRUX_SKIP=1        Disable evaluation for a single command or in the hook.
  GIT_CRUX_STYLE=plain   Use the plain imperative style instead of Conventional Commits.
`)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "git-crux:", err)
	os.Exit(1)
}
