package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// fmBaseURL is Apple's fm serve Chat Completions endpoint (macOS 27+).
	fmBaseURL = "http://127.0.0.1:1976/v1"
	fmHost    = "127.0.0.1"
	fmPort    = 1976
	fmModel   = "system"
	// fmContextTokens is the on-device model's prompt window. Measured as an
	// 8,132-token prompt ceiling plus a small hidden-framing reserve.
	fmContextTokens = 8192
)

// fmServeWait is how long probeAndStartFM waits for /health after spawning
// `fm serve`. Tests shorten it.
var fmServeWait = 5 * time.Second

// cliModel is the -model flag from `git crux`. Empty in the hook and when the
// flag is omitted. It is an override: a non-system value keeps the OpenAI
// default instead of auto-selecting Apple fm.
var cliModel string

var (
	fmOnce sync.Once
	fmOK   bool
)

func ensureAppleFMOnce() bool {
	fmOnce.Do(func() {
		fmOK = probeAndStartFM()
	})
	return fmOK
}

// ensureAppleFM reports whether Apple's on-device model is ready to serve
// requests, starting `fm serve` if needed. Tests replace it so the rest of the
// suite never talks to a real fm binary.
var ensureAppleFM = ensureAppleFMOnce

// fmCmd is the process/HTTP seam for Apple fm. Tests stub these funcs.
var fmCmd = struct {
	lookPath func(string) (string, error)
	run      func(name string, args ...string) (string, error)
	start    func(bin string) error
	healthy  func() bool
}{
	lookPath: exec.LookPath,
	run:      run,
	start:    startFMServe,
	healthy:  fmServeHealthy,
}

func requestedModel() string {
	if cliModel != "" {
		return cliModel
	}
	return os.Getenv("GIT_CRUX_MODEL")
}

// modelLabel is the name shown in the spinner. Apple's on-device model id is
// `system`; that is opaque in a status line, so we say "Apple fm" instead.
func modelLabel(model string) string {
	if strings.EqualFold(model, fmModel) {
		return "Apple fm"
	}
	return model
}

// prefersNonFMModel reports an explicit model id that is not Apple fm's
// `system`. That is treated as an override to OpenAI (or whatever
// GIT_CRUX_BASE_URL already names).
func prefersNonFMModel() bool {
	m := strings.TrimSpace(requestedModel())
	return m != "" && !strings.EqualFold(m, fmModel)
}

func isFMBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return false
	}
	return u.Port() == strconv.Itoa(fmPort)
}

func fmLicenseAgreed(out string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(out)), "agreed")
}

func fmSystemAvailable(out string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(out)), "system model available")
}

// probeAndStartFM is the production auto-detect path: fm on PATH, license
// accepted, system model available, and `fm serve` reachable (started if not).
func probeAndStartFM() bool {
	bin, err := fmCmd.lookPath("fm")
	if err != nil {
		return false
	}
	out, err := fmCmd.run(bin, "license", "--status")
	if err != nil || !fmLicenseAgreed(out) {
		fmt.Fprintln(os.Stderr, "git-crux: Apple fm is installed but its license is not accepted; run sudo fm license to use the on-device model")
		return false
	}
	out, err = fmCmd.run(bin, "available")
	if err != nil || !fmSystemAvailable(out) {
		fmt.Fprintln(os.Stderr, "git-crux: Apple fm is installed but the on-device model is not available; turn on Apple Intelligence in System Settings")
		return false
	}
	if fmCmd.healthy() {
		fmt.Fprintln(os.Stderr, "git-crux: using on-device Apple Foundation Model")
		return true
	}
	if err := fmCmd.start(bin); err != nil {
		fmt.Fprintf(os.Stderr, "git-crux: could not start fm serve: %v\n", err)
		return false
	}
	if waitFMHealthy() {
		fmt.Fprintln(os.Stderr, "git-crux: using on-device Apple Foundation Model")
		return true
	}
	fmt.Fprintln(os.Stderr, "git-crux: started fm serve but it did not become ready")
	return false
}

func waitFMHealthy() bool {
	deadline := time.Now().Add(fmServeWait)
	for {
		if fmCmd.healthy() {
			return true
		}
		if time.Until(deadline) <= 0 {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func fmServeHealthy() bool {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get("http://" + fmHost + ":" + strconv.Itoa(fmPort) + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var h struct {
		Status string `json:"status"`
		Models []struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&h) != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(h.Status), "fm serve") {
		return false
	}
	for _, m := range h.Models {
		if strings.EqualFold(m.Name, fmModel) && m.Available {
			return true
		}
	}
	return false
}

func startFMServe(bin string) error {
	cmd := exec.Command(bin, "serve", "--host", fmHost, "--port", strconv.Itoa(fmPort))
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
