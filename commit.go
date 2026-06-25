package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// runCommit handles `git crux -m "..."`: evaluate the message against the
// staged diff, optionally refine it, then create the commit.
func runCommit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crux", flag.ExitOnError)
	msg := fs.String("m", "", "commit message (omit to have git-crux generate one)")
	model := fs.String("model", modelName(), "model to use")
	noAI := fs.Bool("no-ai", false, "skip AI evaluation, commit as-is")
	dryRun := fs.Bool("dry-run", false, "print the verdict as JSON and exit without committing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	diff, err := stagedDiff()
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		return fmt.Errorf("no staged changes to commit (did you forget to `git add`?)")
	}

	if *dryRun {
		v, err := evaluate(ctx, *msg, diff, *model)
		if err != nil {
			return err
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	// No message given: `git crux` means "generate the commit message for me".
	if strings.TrimSpace(*msg) == "" {
		if *noAI || os.Getenv("GIT_CRUX_SKIP") != "" {
			return fmt.Errorf(`a commit message is required when AI is disabled: git crux -m "..."`)
		}
		return generateAndCommit(ctx, diff, *model)
	}

	final := *msg
	if !*noAI && os.Getenv("GIT_CRUX_SKIP") == "" {
		final = refine(ctx, *msg, diff, *model)
	}

	return commit(final)
}

// generateAndCommit has the model write a commit message for the staged diff
// (with no seed message), lets an interactive user accept or edit it, then
// commits. Non-interactively it commits the generated message as-is.
func generateAndCommit(ctx context.Context, diff, model string) error {
	v, err := evaluate(ctx, "", diff, model)
	if err != nil {
		return fmt.Errorf("generating commit message: %w", err)
	}
	message := strings.TrimSpace(v.Suggestion)
	if message == "" {
		return fmt.Errorf("the model did not return a commit message")
	}
	if isInteractive() {
		message = confirmGenerated(message)
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("commit aborted")
		}
	}
	return commit(message)
}

// refine evaluates the message and, if it is off-point, prompts the user.
// It always FAILS OPEN: any error returns the original message unchanged.
func refine(ctx context.Context, original, diff, model string) string {
	v, err := evaluate(ctx, original, diff, model)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-crux:", err, "(committing as-is; set GIT_CRUX_SKIP=1 to skip checks)")
		return original
	}
	if v.Verdict == "accurate" || strings.TrimSpace(v.Suggestion) == "" {
		return original
	}
	if !isInteractive() {
		return original
	}
	return promptUser(original, v)
}
