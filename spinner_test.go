package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestStartSpinnerNonInteractive verifies the spinner is a no-op without a usable
// terminal, so it never writes escape codes in CI or piped/hook contexts.
func TestStartSpinnerNonInteractive(t *testing.T) {
	t.Setenv("CI", "1") // isInteractive() returns false when CI is set
	if sp := startSpinner("asking model"); sp != nil {
		sp.Stop()
		t.Error("expected a nil (no-op) spinner in a non-interactive context")
	}
}

// TestSpinnerStopNil confirms Stop is safe on the nil handle callers defer.
func TestSpinnerStopNil(t *testing.T) {
	var sp *spinner
	sp.Stop() // must not panic
}

// TestSpinnerLifecycle drives the animation against a temp file standing in for
// the tty: it must render the label, clear its line on stop, and tolerate a
// second Stop without panicking or blocking.
func TestSpinnerLifecycle(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "spin")
	if err != nil {
		t.Fatal(err)
	}
	s := &spinner{tty: f, stop: make(chan struct{}), done: make(chan struct{})}
	go s.run("asking test-model")
	time.Sleep(200 * time.Millisecond) // allow a couple of frames at 80ms
	s.Stop()
	s.Stop() // idempotent

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, "asking test-model") {
		t.Errorf("spinner output missing label: %q", out)
	}
	if !strings.Contains(out, "\033[K") {
		t.Error("spinner did not clear its line on stop")
	}
}
