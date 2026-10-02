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
	if sp := startSpinner("asking model", time.Minute); sp != nil {
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
	go s.run("asking test-model", 90*time.Second)
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
	if !strings.Contains(out, "0s / 1m30s") {
		t.Errorf("spinner output missing elapsed time against the limit: %q", out)
	}
	if !strings.Contains(out, "\033[K") {
		t.Error("spinner did not clear its line on stop")
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{1500 * time.Millisecond, "1s"}, // truncated, never rounded up past the truth
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{2*time.Minute + 5*time.Second, "2m05s"}, // seconds padded, as on a clock
		{2*time.Minute + 14*time.Second + 380*time.Millisecond, "2m14s"},
		{10 * time.Minute, "10m"},
		{90 * time.Second, "1m30s"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Minute + 9*time.Second, "1h5m"},
	} {
		if got := shortDuration(tc.d); got != tc.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestProgress(t *testing.T) {
	if got := progress(134*time.Second, 10*time.Minute); got != "2m14s / 10m" {
		t.Errorf("got %q", got)
	}
	if got := progress(7*time.Second, 0); got != "7s" {
		t.Errorf("no limit: got %q, want the elapsed time alone", got)
	}
}
