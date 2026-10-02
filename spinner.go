package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// spinnerFrames are braille cells that read as a single rotating dot.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner animates a one-line status indicator on the controlling terminal while
// a slow operation (the model call) runs. It writes to /dev/tty — never stdout —
// so it never corrupts -dry-run JSON, the hook's message file, or piped output,
// and it clears its line when stopped.
type spinner struct {
	tty  *os.File
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startSpinner begins animating label and returns a handle to stop it. It returns
// nil — a no-op handle — when there is no usable terminal (non-interactive
// contexts, CI, or a hook without a tty), so callers can unconditionally
// `defer sp.Stop()`. The first frame is delayed by one tick, so a fast response
// completes before anything is drawn.
//
// The line also shows the time spent so far against limit, the request's
// timeout (omitted when limit is 0). A slow local model can take minutes per
// reply, and a spinner that looks the same at second 5 and minute 5 reads as
// hung; the count shows it is still within its budget.
func startSpinner(label string, limit time.Duration) *spinner {
	if !isInteractive() {
		return nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	s := &spinner{
		tty:  tty,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.run(label, limit)
	return s
}

func (s *spinner) run(label string, limit time.Duration) {
	defer close(s.done)
	fmt.Fprint(s.tty, "\033[?25l")       // hide cursor
	defer fmt.Fprint(s.tty, "\033[?25h") // restore cursor

	start := time.Now()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			// \033[K clears whatever a longer previous frame left behind.
			fmt.Fprintf(s.tty, "\r%s %s  %s\033[K", spinnerFrames[i%len(spinnerFrames)], label, progress(time.Since(start), limit))
		}
	}
}

// progress renders elapsed against limit, e.g. "2m14s / 10m", or elapsed alone
// when there is no limit.
func progress(elapsed, limit time.Duration) string {
	if limit <= 0 {
		return shortDuration(elapsed)
	}
	return shortDuration(elapsed) + " / " + shortDuration(limit)
}

// shortDuration formats d in whole seconds the way a person would write it:
// "7s", "2m14s", "10m", "1h5m". time.Duration's own String gives "10m0s" and,
// before truncation, sub-second noise like "2m14.38s".
func shortDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && sec > 0:
		return fmt.Sprintf("%dm%02ds", m, sec)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// Stop halts the animation, clears its line, and restores the cursor. It is safe
// to call on a nil spinner and safe to call more than once.
func (s *spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		fmt.Fprint(s.tty, "\r\033[K") // carriage return, clear to end of line
		s.tty.Close()
	})
}
