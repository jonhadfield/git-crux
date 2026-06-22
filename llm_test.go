package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextTokensKnownModel(t *testing.T) {
	cases := map[string]int{
		"microsoft/phi-4":     16384,
		"Phi-4-mini":          16384, // case-insensitive substring match
		"qwen2.5-coder:7b":    32768,
		"llama-3.1-8b":        131072,
		"llama-3-8b-instruct": 8192,
		"gpt-4o":              128000,
	}
	for model, want := range cases {
		if got := contextTokens(model); got != want {
			t.Errorf("contextTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestContextTokensUnknownModel(t *testing.T) {
	if got := contextTokens("some-obscure-model"); got != defaultContextTokens {
		t.Errorf("unknown model = %d, want default %d", got, defaultContextTokens)
	}
}

func TestContextTokensEnvOverride(t *testing.T) {
	t.Setenv("GIT_CRUX_CONTEXT", "4096")
	if got := contextTokens("microsoft/phi-4"); got != 4096 {
		t.Errorf("env override = %d, want 4096", got)
	}
}

func TestDiffBudgetScalesWithModel(t *testing.T) {
	small := diffBudget("phi-3")  // 4096 ctx
	large := diffBudget("gpt-4o") // 128000 ctx
	if small >= large {
		t.Errorf("budget did not scale: small(phi-3)=%d large(gpt-4o)=%d", small, large)
	}
	if small < minDiffChars {
		t.Errorf("small budget %d below floor %d", small, minDiffChars)
	}
	if large > maxDiffCharsCeil {
		t.Errorf("large budget %d above ceiling %d", large, maxDiffCharsCeil)
	}
}

func TestDiffBudgetFloorAndCeiling(t *testing.T) {
	t.Setenv("GIT_CRUX_CONTEXT", "1000") // tiny window → would compute below floor
	if got := diffBudget("anything"); got != minDiffChars {
		t.Errorf("tiny context = %d, want floor %d", got, minDiffChars)
	}
}

func TestDiffBudgetEnvOverrideWins(t *testing.T) {
	t.Setenv("GIT_CRUX_MAX_DIFF", "5000")
	t.Setenv("GIT_CRUX_CONTEXT", "128000") // ignored when MAX_DIFF is set
	if got := diffBudget("gpt-4o"); got != 5000 {
		t.Errorf("GIT_CRUX_MAX_DIFF override = %d, want 5000", got)
	}
}

func TestEnvIntRejectsJunk(t *testing.T) {
	t.Setenv("GIT_CRUX_CONTEXT", "not-a-number")
	if _, ok := envInt("GIT_CRUX_CONTEXT"); ok {
		t.Error("envInt accepted non-numeric value")
	}
	t.Setenv("GIT_CRUX_CONTEXT", "-5")
	if _, ok := envInt("GIT_CRUX_CONTEXT"); ok {
		t.Error("envInt accepted non-positive value")
	}
}

func TestIsContextOverflow(t *testing.T) {
	overflow := []string{
		"model server returned 400 Bad Request: context length exceeded",
		"n_keep: 9700 >= n_ctx: 8192",
		"prompt has too many tokens for the maximum context",
	}
	for _, m := range overflow {
		if !isContextOverflow(errString(m)) {
			t.Errorf("expected overflow detection for %q", m)
		}
	}
	other := []string{"connection refused", "model returned no choices", "500 Internal Server Error: boom"}
	for _, m := range other {
		if isContextOverflow(errString(m)) {
			t.Errorf("misclassified %q as overflow", m)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestEvaluateRetriesOnOverflow drives evaluate against a server that rejects
// the first (oversized) prompt with a context-length 400, then accepts the
// halved retry — proving the budget self-corrects to the real context.
func TestEvaluateRetriesOnOverflow(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"the number of tokens exceeds context length (n_ctx: 8192)"}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"verdict\":\"vague\",\"suggestion\":\"x\",\"reason\":\"y\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)

	diff := strings.Repeat("diff --git a/f.go b/f.go\n+line\n", 4000) // large enough to halve and still exceed the floor
	v, err := evaluate("msg", diff, "microsoft/phi-4")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least one retry, got %d call(s)", calls)
	}
	if v.Verdict != "vague" {
		t.Errorf("verdict = %q, want vague", v.Verdict)
	}
}

// TestEvaluateNoRetryOnOtherErrors ensures a non-context error fails fast.
func TestEvaluateNoRetryOnOtherErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)

	if _, err := evaluate("msg", "diff --git a/f b/f\n+x\n", "microsoft/phi-4"); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected no retry, got %d calls", calls)
	}
}

func TestDefaultProfileWithKey(t *testing.T) {
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", "")
	t.Setenv("GIT_CRUX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := baseURL(); got != defaultBaseURL {
		t.Errorf("baseURL with key = %q, want %q", got, defaultBaseURL)
	}
	if got := modelName(); got != defaultModel {
		t.Errorf("modelName with key = %q, want %q", got, defaultModel)
	}
}

func TestLocalFallbackWithoutKey(t *testing.T) {
	t.Setenv("GIT_CRUX_BASE_URL", "")
	t.Setenv("GIT_CRUX_MODEL", "")
	t.Setenv("GIT_CRUX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if got := baseURL(); got != localBaseURL {
		t.Errorf("baseURL without key = %q, want local %q", got, localBaseURL)
	}
	if got := modelName(); got != localModel {
		t.Errorf("modelName without key = %q, want local %q", got, localModel)
	}
}

func TestExplicitLocalURLKeepsLocalModel(t *testing.T) {
	// A user with a key who points at a local server should still get the local
	// model default, not gpt-4o-mini sent to localhost.
	t.Setenv("GIT_CRUX_BASE_URL", "http://localhost:1234/v1")
	t.Setenv("GIT_CRUX_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := modelName(); got != localModel {
		t.Errorf("modelName with explicit local URL = %q, want %q", got, localModel)
	}
}
