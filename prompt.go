package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// isInteractive reports whether we can prompt the user. It returns false in CI
// and whenever the controlling terminal is unavailable (scripts, GUIs, hooks
// run without a tty), so automated commits are never blocked.
func isInteractive() bool {
	if os.Getenv("CI") != "" {
		return false
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// confirmGenerated shows a freshly generated commit message and lets the user
// accept it, replace it with their own line, or cancel. Returns the chosen
// message, or "" to abort the commit. Reads from /dev/tty like promptUser.
func confirmGenerated(message string) string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return message // can't prompt; use the generated message as-is
	}
	defer tty.Close()

	if strings.Contains(strings.TrimSpace(message), "\n") {
		fmt.Fprintf(tty, "\n  generated message:\n%s\n\n", indent(message, "    "))
	} else {
		fmt.Fprintf(tty, "\n  generated message:  %s\n\n", message)
	}
	fmt.Fprint(tty, "  [A]ccept / [e]dit / [c]ancel? ")

	reader := bufio.NewReader(tty)
	choice, _ := reader.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(choice)) {
	case "", "a":
		return message
	case "e":
		fmt.Fprint(tty, "  new message: ")
		edited, _ := reader.ReadString('\n')
		if edited = strings.TrimSpace(edited); edited != "" {
			return edited
		}
		return message
	default:
		return "" // cancel
	}
}

// promptUser shows the verdict and suggestion and returns the chosen message.
// It reads from /dev/tty directly because git hooks do not connect stdin to the
// terminal. On any read error it falls back to the original message.
func promptUser(original string, v *verdict) string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return original
	}
	defer tty.Close()

	fmt.Fprintf(tty, "\n  message:  %s\n", original)
	fmt.Fprintf(tty, "  verdict:  %s — %s\n", v.Verdict, v.Reason)
	if strings.Contains(strings.TrimSpace(v.Suggestion), "\n") {
		fmt.Fprintf(tty, "  suggest:\n%s\n", indent(v.Suggestion, "    "))
	} else {
		fmt.Fprintf(tty, "  suggest:  %s\n\n", v.Suggestion)
	}
	fmt.Fprint(tty, "  [A]ccept suggestion / [e]dit / [k]eep original? ")

	reader := bufio.NewReader(tty)
	choice, _ := reader.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(choice)) {
	case "", "a":
		return v.Suggestion
	case "e":
		fmt.Fprint(tty, "  new message: ")
		edited, _ := reader.ReadString('\n')
		if edited = strings.TrimSpace(edited); edited != "" {
			return edited
		}
		return original
	default:
		return original
	}
}
