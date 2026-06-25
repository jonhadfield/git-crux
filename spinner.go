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
func startSpinner(label string) *spinner {
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
	go s.run(label)
	return s
}

func (s *spinner) run(label string) {
	defer close(s.done)
	fmt.Fprint(s.tty, "\033[?25l")       // hide cursor
	defer fmt.Fprint(s.tty, "\033[?25h") // restore cursor

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			fmt.Fprintf(s.tty, "\r%s %s", spinnerFrames[i%len(spinnerFrames)], label)
		}
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
