package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Keep the rest of the suite off the real fm binary: auto-detect would
	// otherwise start `fm serve` on any macOS 27 developer machine.
	ensureAppleFM = func() bool { return false }
	os.Exit(m.Run())
}

func TestFMLicenseAgreed(t *testing.T) {
	if !fmLicenseAgreed("Agreed to license FM1 version 1.0 on 24 Oct 2026 at 11:58.\n") {
		t.Fatal("accepted status should count as agreed")
	}
	if fmLicenseAgreed("Not agreed to the Apple Foundation Models CLI Legal Notice & Terms.") {
		t.Fatal("unaccepted status should not count as agreed")
	}
	if fmLicenseAgreed("") {
		t.Fatal("empty status should not count as agreed")
	}
}

func TestFMSystemAvailable(t *testing.T) {
	if !fmSystemAvailable("System model available\n") {
		t.Fatal("ready output should count as available")
	}
	if fmSystemAvailable("System model unavailable: Apple Intelligence is not enabled.") {
		t.Fatal("unavailable output should not count as available")
	}
}

func TestModelLabel(t *testing.T) {
	if got := modelLabel("system"); got != "Apple fm" {
		t.Errorf("modelLabel(system) = %q, want Apple fm", got)
	}
	if got := modelLabel("SYSTEM"); got != "Apple fm" {
		t.Errorf("modelLabel(SYSTEM) = %q, want Apple fm", got)
	}
	if got := modelLabel("gpt-4o-mini"); got != "gpt-4o-mini" {
		t.Errorf("modelLabel(gpt-4o-mini) = %q, want the model id unchanged", got)
	}
}

func TestIsFMBaseURL(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:1976/v1",
		"http://localhost:1976/v1",
		"http://[::1]:1976/v1",
	} {
		if !isFMBaseURL(u) {
			t.Errorf("%s should be recognized as fm serve", u)
		}
	}
	for _, u := range []string{
		"http://localhost:1234/v1",
		"http://127.0.0.1:11434/v1",
		"https://api.openai.com/v1",
		"not a url",
	} {
		if isFMBaseURL(u) {
			t.Errorf("%s should not be recognized as fm serve", u)
		}
	}
}

func TestProbeSkipsWhenFMMissing(t *testing.T) {
	stubFM(t, fmStub{lookErr: errors.New("not found")})
	if probeAndStartFM() {
		t.Fatal("missing fm binary should not select Apple fm")
	}
}

func TestProbeSkipsWhenLicenseNotAccepted(t *testing.T) {
	var started bool
	stubFM(t, fmStub{
		bin:     "/usr/bin/fm",
		license: "Not agreed.",
		start:   func() error { started = true; return nil },
	})
	stderr := captureStderr(t, func() {
		if probeAndStartFM() {
			t.Error("unaccepted license should not select Apple fm")
		}
	})
	if started {
		t.Error("must not start fm serve before the license is accepted")
	}
	if !strings.Contains(stderr, "sudo fm license") {
		t.Errorf("stderr should tell the user how to accept the license, got %q", stderr)
	}
}

func TestProbeSkipsWhenModelUnavailable(t *testing.T) {
	var started bool
	stubFM(t, fmStub{
		bin:       "/usr/bin/fm",
		license:   "Agreed to license FM1 version 1.0 on 24 Oct 2026 at 11:58.",
		available: "System model unavailable",
		start:     func() error { started = true; return nil },
	})
	stderr := captureStderr(t, func() {
		if probeAndStartFM() {
			t.Error("unavailable system model should not select Apple fm")
		}
	})
	if started {
		t.Error("must not start fm serve when the model is not available")
	}
	if !strings.Contains(stderr, "Apple Intelligence") {
		t.Errorf("stderr should mention Apple Intelligence, got %q", stderr)
	}
}

func TestProbeUsesExistingServe(t *testing.T) {
	var started bool
	stubFM(t, fmStub{
		bin:       "/usr/bin/fm",
		license:   "Agreed to license FM1 version 1.0 on 24 Oct 2026 at 11:58.",
		available: "System model available",
		healthy:   true,
		start:     func() error { started = true; return nil },
	})
	stderr := captureStderr(t, func() {
		if !probeAndStartFM() {
			t.Error("ready fm serve should be selected")
		}
	})
	if started {
		t.Error("must not restart fm serve when /health is already ok")
	}
	if !strings.Contains(stderr, "on-device Apple Foundation Model") {
		t.Errorf("stderr should say fm is in use, got %q", stderr)
	}
}

func TestProbeStartsServeWhenNeeded(t *testing.T) {
	var started bool
	healthy := false
	stubFM(t, fmStub{
		bin:       "/usr/bin/fm",
		license:   "Agreed to license FM1 version 1.0 on 24 Oct 2026 at 11:58.",
		available: "System model available",
		healthyFn: func() bool { return healthy },
		start: func() error {
			started = true
			healthy = true
			return nil
		},
	})
	captureStderr(t, func() {
		if !probeAndStartFM() {
			t.Error("starting fm serve should select Apple fm once /health succeeds")
		}
	})
	if !started {
		t.Error("expected fm serve to be started")
	}
}

func TestProbeGivesUpWhenServeNeverBecomesReady(t *testing.T) {
	fmServeWait = 80 * time.Millisecond
	t.Cleanup(func() { fmServeWait = 5 * time.Second })
	stubFM(t, fmStub{
		bin:       "/usr/bin/fm",
		license:   "Agreed to license FM1 version 1.0 on 24 Oct 2026 at 11:58.",
		available: "System model available",
		healthy:   false,
		start:     func() error { return nil },
	})
	captureStderr(t, func() {
		if probeAndStartFM() {
			t.Error("unready fm serve should not be selected")
		}
	})
}

func TestBaseURLPrefersFMWhenReady(t *testing.T) {
	useAppleFM(t, true)
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := baseURL(); got != fmBaseURL {
		t.Errorf("baseURL = %q, want Apple fm %q even with an OpenAI key set", got, fmBaseURL)
	}
	if got := modelName(); got != fmModel {
		t.Errorf("modelName = %q, want %q", got, fmModel)
	}
	if err := missingAPIKeyError(); err != nil {
		t.Errorf("Apple fm should not require an API key, got %v", err)
	}
}

func TestBaseURLKeepsOpenAIWhenFMUnavailable(t *testing.T) {
	useAppleFM(t, false)
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", "")
	if got := baseURL(); got != defaultBaseURL {
		t.Errorf("baseURL = %q, want OpenAI default %q", got, defaultBaseURL)
	}
	if got := modelName(); got != defaultModel {
		t.Errorf("modelName = %q, want %q", got, defaultModel)
	}
}

func TestBaseURLRespectsExplicitServer(t *testing.T) {
	var called bool
	ensureAppleFM = func() bool { called = true; return true }
	t.Cleanup(func() { ensureAppleFM = func() bool { return false } })
	t.Setenv("GIT_CRUX_BASE_URL", localBaseURL)
	t.Setenv("GIT_CRUX_MODEL", "")
	if got := baseURL(); got != strings.TrimRight(localBaseURL, "/") {
		t.Errorf("baseURL = %q, want explicit local %q", got, localBaseURL)
	}
	if called {
		t.Error("explicit non-fm URL must not start fm serve")
	}
	if got := modelName(); got != localModel {
		t.Errorf("modelName = %q, want %q", got, localModel)
	}
}

func TestBaseURLRespectsOpenAIModelOverride(t *testing.T) {
	useAppleFM(t, true)
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", defaultModel)
	if got := baseURL(); got != defaultBaseURL {
		t.Errorf("GIT_CRUX_MODEL=%s should keep OpenAI, got %q", defaultModel, got)
	}
	if got := modelName(); got != defaultModel {
		t.Errorf("modelName = %q, want %q", got, defaultModel)
	}
}

func TestBaseURLRespectsCLIModelOverride(t *testing.T) {
	useAppleFM(t, true)
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", "")
	cliModel = defaultModel
	t.Cleanup(func() { cliModel = "" })
	if got := baseURL(); got != defaultBaseURL {
		t.Errorf("-model %s should keep OpenAI, got %q", defaultModel, got)
	}
}

func TestExplicitFMURLEnsuresServe(t *testing.T) {
	var called bool
	ensureAppleFM = func() bool { called = true; return true }
	t.Cleanup(func() { ensureAppleFM = func() bool { return false } })
	t.Setenv("GIT_CRUX_BASE_URL", fmBaseURL)
	t.Setenv("GIT_CRUX_MODEL", "")
	if got := baseURL(); got != fmBaseURL {
		t.Errorf("baseURL = %q, want %q", got, fmBaseURL)
	}
	if !called {
		t.Error("an explicit fm URL should still start serve if needed")
	}
	if got := modelName(); got != fmModel {
		t.Errorf("modelName for fm URL = %q, want %q", got, fmModel)
	}
}

func TestConnectHintFM(t *testing.T) {
	if got, want := connectHint(fmBaseURL), "is fm serve running?"; got != want {
		t.Errorf("connectHint(fm) = %q, want %q", got, want)
	}
}

type fmStub struct {
	bin        string
	lookErr    error
	license    string
	licenseErr error
	available  string
	availErr   error
	healthy    bool
	healthyFn  func() bool
	start      func() error
}

func stubFM(t *testing.T, s fmStub) {
	t.Helper()
	orig := fmCmd
	t.Cleanup(func() { fmCmd = orig })
	fmCmd.lookPath = func(string) (string, error) {
		if s.lookErr != nil {
			return "", s.lookErr
		}
		return s.bin, nil
	}
	fmCmd.run = func(_ string, args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "license" {
			return s.license, s.licenseErr
		}
		return s.available, s.availErr
	}
	fmCmd.start = func(string) error {
		if s.start != nil {
			return s.start()
		}
		return nil
	}
	fmCmd.healthy = func() bool {
		if s.healthyFn != nil {
			return s.healthyFn()
		}
		return s.healthy
	}
}

func useAppleFM(t *testing.T, ready bool) {
	t.Helper()
	ensureAppleFM = func() bool { return ready }
	t.Cleanup(func() { ensureAppleFM = func() bool { return false } })
}
