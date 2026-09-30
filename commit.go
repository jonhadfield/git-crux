package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runCommit handles `git crux -m "..."`: evaluate the message against the
// staged diff, optionally refine it, then create the commit.
func runCommit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crux", flag.ExitOnError)
	msg := fs.String("m", "", "commit message (omit to have git-crux generate one)")
	model := fs.String("model", modelName(), "model to use")
	styleFlag := fs.String("style", "", "message style: conventional|plain (default conventional)")
	noAI := fs.Bool("no-ai", false, "skip AI evaluation, commit as-is")
	dryRun := fs.Bool("dry-run", false, "print the verdict as JSON and exit without committing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	style := commitStyle(*styleFlag)

	diff, err := stagedDiff()
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		return fmt.Errorf("no staged changes to commit (did you forget to `git add`?)")
	}

	if *dryRun {
		v, err := evaluate(ctx, *msg, diff, *model, style)
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
		return generateAndCommit(ctx, diff, *model, style)
	}

	final := *msg
	if !*noAI && os.Getenv("GIT_CRUX_SKIP") == "" {
		final, err = refine(ctx, *msg, diff, *model, style)
		if err != nil {
			return err
		}
	}

	return commit(final)
}

// generateAndCommit has the model write a commit message for the staged diff
// (with no seed message), lets an interactive user accept or edit it, then
// commits. Non-interactively it commits the generated message as-is.
// When the model is unreachable interactively, the user can write their own
// message or abort; non-interactively the error is returned.
func generateAndCommit(ctx context.Context, diff, model, style string) error {
	v, err := evaluate(ctx, "", diff, model, style)
	if err != nil {
		return fallbackGenerate(fmt.Errorf("generating commit message: %w", err))
	}
	message := strings.TrimSpace(v.Suggestion)
	if message == "" {
		return fallbackGenerate(fmt.Errorf("the model did not return a commit message"))
	}
	if isInteractive() {
		message = confirmGenerated(message)
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("commit aborted")
		}
	}
	return commit(message)
}

// fallbackGenerate offers an interactive stop-or-write-your-own path when
// generation fails; otherwise returns err unchanged.
func fallbackGenerate(err error) error {
	if !isInteractive() {
		return err
	}
	message := promptOwnMessage(err)
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("commit aborted")
	}
	return commit(message)
}

// refine evaluates the message and, if it is off-point, prompts the user.
// On AI failure: interactively offers continue-with-original or abort;
// non-interactively fails open and returns the original unchanged.
func refine(ctx context.Context, original, diff, model, style string) (string, error) {
	v, err := evaluate(ctx, original, diff, model, style)
	if err != nil {
		if isInteractive() {
			if !promptContinueOrAbort(err) {
				return "", fmt.Errorf("commit aborted")
			}
			return original, nil
		}
		fmt.Fprintln(os.Stderr, "git-crux:", err, "(committing as-is; set GIT_CRUX_SKIP=1 to skip checks)")
		return original, nil
	}
	if v.Verdict == "accurate" || strings.TrimSpace(v.Suggestion) == "" {
		return original, nil
	}
	if !isInteractive() {
		noteUnusedSuggestion(os.Stderr, v)
		return original, nil
	}
	return promptUser(original, v), nil
}

// noteUnusedSuggestion reports a verdict that nobody could be asked about. With
// no terminal the original message stands, which is right, but doing it
// silently threw away a review that could take minutes on a local model, and a
// `git crux -m` run from a script looked as though the message had passed.
func noteUnusedSuggestion(w io.Writer, v *verdict) {
	fmt.Fprintf(w, "git-crux: message looks %s", v.Verdict)
	if r := strings.TrimSpace(v.Reason); r != "" {
		fmt.Fprintf(w, " (%s)", r)
	}
	fmt.Fprintln(w, "; no terminal to ask on, so committing as-is. Suggested:")
	for _, line := range strings.Split(v.Suggestion, "\n") {
		fmt.Fprintln(w, strings.TrimRight("    "+line, " "))
	}
}
