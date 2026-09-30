package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNoteUnusedSuggestion(t *testing.T) {
	var b strings.Builder
	noteUnusedSuggestion(&b, &verdict{
		Verdict:    "vague",
		Suggestion: "chore: update gateway\n\n- Bump gateway module to v1.1.39",
		Reason:     "missing type prefix",
	})
	want := "git-crux: message looks vague (missing type prefix); no terminal to ask on, so committing as-is. Suggested:\n" +
		"    chore: update gateway\n" +
		"\n" + // the blank line between subject and body carries no trailing indent
		"    - Bump gateway module to v1.1.39\n"
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNoteUnusedSuggestionWithoutReason(t *testing.T) {
	var b strings.Builder
	noteUnusedSuggestion(&b, &verdict{Verdict: "wrong", Suggestion: "fix: x"})
	if got := b.String(); !strings.HasPrefix(got, "git-crux: message looks wrong; ") {
		t.Errorf("an empty reason should leave no empty parentheses, got %q", got)
	}
}

// Observed running `git crux -m "update gateway"` without a terminal: qwen
// judged it vague and suggested a replacement, and the commit went through with
// nothing printed. The original must still be committed, but not silently.
func TestRefineNonInteractiveReportsSuggestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"vague\",\"suggestion\":\"chore: update gateway to v1.1.39\",\"reason\":\"generic\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_CONTEXT", "8192")
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")
	t.Setenv("CI", "1") // forces isInteractive() false

	var msg string
	var err error
	stderr := captureStderr(t, func() {
		msg, err = refine(context.Background(), "update gateway", "diff --git a/main.tf b/main.tf\n+x\n", "m", styleConventional)
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "update gateway" {
		t.Errorf("message = %q, want the original unchanged", msg)
	}
	for _, want := range []string{"looks vague", "committing as-is", "chore: update gateway to v1.1.39"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q should contain %q", stderr, want)
		}
	}
}

func TestRefineNonInteractiveQuietWhenAccurate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"accurate\",\"suggestion\":\"\",\"reason\":\"ok\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_CONTEXT", "8192")
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")
	t.Setenv("CI", "1")

	stderr := captureStderr(t, func() {
		if _, err := refine(context.Background(), "feat: add x", "diff --git a/x b/x\n+x\n", "m", styleConventional); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(stderr, "Suggested") {
		t.Errorf("an accurate verdict should print no suggestion, got %q", stderr)
	}
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}
