package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// stagedDiff returns the diff of staged changes (what `git commit` would record).
func stagedDiff() (string, error) {
	return run("git", "diff", "--cached")
}

// commit creates a commit with the given message. GIT_CRUX_SKIP is set so that,
// if the prepare-commit-msg hook is installed, it does not re-evaluate and
// double-prompt for this same message.
func commit(message string) error {
	cmd := exec.Command("git", "commit", "-m", message)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GIT_CRUX_SKIP=1")
	return cmd.Run()
}

// run executes a command and returns its stdout, surfacing stderr on failure.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
