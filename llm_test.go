package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestEvaluateRetriesOnOverflow drives the whole-diff path against a server that
// rejects the first prompt with a context-length 400, then accepts the halved
// retry — proving the budget self-corrects to the model's real context. The diff
// is kept under the budget so it takes evaluateWhole, not the chunked path.
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

	// Single file, ~18KB: under phi-4's 32KB budget (so evaluateWhole runs) but
	// large enough that one halving still clears the floor.
	diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -0,0 +1 @@\n" + strings.Repeat("+line\n", 3000)
	v, err := evaluate(context.Background(), "msg", diff, "microsoft/phi-4", stylePlain)
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

// TestEvaluateChunkedFlow forces the chunked path with a tiny budget and proves
// the map-reduce shape: several summarize calls (no schema) feed one final
// verdict call (with schema), whose result is returned.
func TestEvaluateChunkedFlow(t *testing.T) {
	var summarizeCalls, verdictCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "json_schema") {
			verdictCalls++
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"verdict\":\"vague\",\"suggestion\":\"feat: x\",\"reason\":\"y\"}"}}]}`)
			return
		}
		summarizeCalls++
		io.WriteString(w, `{"choices":[{"message":{"content":"- changed thing"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_MAX_DIFF", "200") // tiny budget → force chunking

	// Three files, each on its own chunk under the 200-byte budget.
	diff := makeFileDiff("a.go", 80) + makeFileDiff("b.go", 80) + makeFileDiff("c.go", 80)
	v, err := evaluate(context.Background(), "msg", diff, "gpt-4o", styleConventional)
	if err != nil {
		t.Fatalf("chunked evaluate failed: %v", err)
	}
	if summarizeCalls < 2 {
		t.Errorf("expected multiple summarize calls, got %d", summarizeCalls)
	}
	if verdictCalls != 1 {
		t.Errorf("expected exactly one verdict call, got %d", verdictCalls)
	}
	if v.Verdict != "vague" || v.Suggestion != "feat: x" {
		t.Errorf("unexpected verdict: %+v", v)
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

	if _, err := evaluate(context.Background(), "msg", "diff --git a/f b/f\n+x\n", "microsoft/phi-4", stylePlain); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected no retry, got %d calls", calls)
	}
}

// TestParseVerdict covers the messy shapes a local model may wrap its JSON in:
// bare object, markdown fences, and surrounding prose.
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    verdict
	}{
		{
			name:    "bare object",
			content: `{"verdict":"vague","suggestion":"Do the thing","reason":"too generic"}`,
			want:    verdict{"vague", "Do the thing", "too generic"},
		},
		{
			name:    "markdown fenced",
			content: "```json\n{\"verdict\":\"accurate\",\"suggestion\":\"\",\"reason\":\"matches\"}\n```",
			want:    verdict{"accurate", "", "matches"},
		},
		{
			name:    "wrapped in prose",
			content: `Sure! Here is the verdict: {"verdict":"wrong","suggestion":"Fix it","reason":"mismatch"} Hope that helps.`,
			want:    verdict{"wrong", "Fix it", "mismatch"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseVerdict(c.content)
			if err != nil {
				t.Fatalf("parseVerdict(%q) errored: %v", c.content, err)
			}
			if *got != c.want {
				t.Errorf("parseVerdict(%q) = %+v, want %+v", c.content, *got, c.want)
			}
		})
	}
}

func TestParseVerdictRejectsNonJSON(t *testing.T) {
	if _, err := parseVerdict("I cannot help with that."); err == nil {
		t.Error("expected error for content with no JSON object")
	}
}

// TestEvaluateRetriesOnTransientNetworkError drops the first connection before
// any response (a transport error), then answers the retry — proving a one-off
// network blip self-heals rather than failing the whole evaluation.
func TestEvaluateRetriesOnTransientNetworkError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter is not a Hijacker")
			}
			conn, _, _ := hj.Hijack()
			conn.Close() // no response → client sees a transport error
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"verdict\":\"accurate\",\"suggestion\":\"\",\"reason\":\"ok\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)

	v, err := evaluate(context.Background(), "msg", "diff --git a/f b/f\n+x\n", "gpt-4o", stylePlain)
	if err != nil {
		t.Fatalf("expected success after transient retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 calls (1 fail + 1 retry), got %d", calls)
	}
	if v.Verdict != "accurate" {
		t.Errorf("verdict = %q, want accurate", v.Verdict)
	}
}

func TestCommitStyle(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("GIT_CRUX_STYLE", "conventional")
		if got := commitStyle("plain"); got != stylePlain {
			t.Errorf("flag override = %q, want %q", got, stylePlain)
		}
	})
	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv("GIT_CRUX_STYLE", "plain")
		if got := commitStyle(""); got != stylePlain {
			t.Errorf("env value = %q, want %q", got, stylePlain)
		}
	})
	t.Run("default is conventional", func(t *testing.T) {
		t.Setenv("GIT_CRUX_STYLE", "")
		if got := commitStyle(""); got != styleConventional {
			t.Errorf("default = %q, want %q", got, styleConventional)
		}
	})
	t.Run("unknown falls back to default", func(t *testing.T) {
		t.Setenv("GIT_CRUX_STYLE", "")
		if got := commitStyle("emoji"); got != defaultStyle {
			t.Errorf("unknown style = %q, want default %q", got, defaultStyle)
		}
	})
	t.Run("case-insensitive", func(t *testing.T) {
		t.Setenv("GIT_CRUX_STYLE", "")
		if got := commitStyle("Plain"); got != stylePlain {
			t.Errorf("mixed case = %q, want %q", got, stylePlain)
		}
	})
}

// TestSystemPromptStyle confirms each style injects its own format rules: the
// conventional prompt names the standard and the type vocabulary; the plain one
// does neither.
func TestSystemPromptStyle(t *testing.T) {
	conv := systemPrompt(styleConventional)
	if !strings.Contains(conv, "Conventional Commits") {
		t.Error("conventional prompt missing the standard's name")
	}
	for _, typ := range []string{"feat:", "fix:", "chore:", "refactor:"} {
		if !strings.Contains(conv, typ) {
			t.Errorf("conventional prompt missing type %q", typ)
		}
	}
	plain := systemPrompt(stylePlain)
	if strings.Contains(plain, "Conventional Commits") {
		t.Error("plain prompt should not mention Conventional Commits")
	}
	// Both styles share the verdict vocabulary.
	for _, p := range []string{conv, plain} {
		if !strings.Contains(p, `"incomplete"`) {
			t.Error("prompt missing shared verdict vocabulary")
		}
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

func TestConnectHint(t *testing.T) {
	// A local endpoint means a missing local server, whatever the proxy says:
	// localhost is bypassed by Go's proxy handling, so the proxy is irrelevant.
	t.Setenv("HTTPS_PROXY", "http://proxy.example.com:3128")
	for _, url := range []string{
		"http://localhost:1234/v1",
		"http://127.0.0.1:1234/v1",
		"http://[::1]:1234/v1",
	} {
		if got, want := connectHint(url), "is your local model server running?"; got != want {
			t.Errorf("connectHint(%q) = %q, want %q", url, got, want)
		}
	}

	// A hosted endpoint with a proxy configured: name the variable to check,
	// never its value, which may carry credentials.
	got := connectHint("https://api.openai.com/v1")
	if want := "check your network connection and the proxy in HTTPS_PROXY"; got != want {
		t.Errorf("proxied hosted endpoint = %q, want %q", got, want)
	}
	if strings.Contains(got, "proxy.example.com") {
		t.Errorf("hint leaks the proxy value: %q", got)
	}
	if strings.Contains(got, "LM Studio") {
		t.Errorf("hosted endpoint should not mention LM Studio: %q", got)
	}
}

func TestConnectHintNoProxy(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(name, "")
	}
	if got, want := connectHint("https://api.openai.com/v1"), "check your network connection"; got != want {
		t.Errorf("connectHint = %q, want %q", got, want)
	}
}

func TestStripThinkBlocks(t *testing.T) {
	cases := map[string]string{
		"<think>weighing it up</think>{\"verdict\":\"vague\"}": "{\"verdict\":\"vague\"}",
		"a<think>one</think>b<think>two</think>c":              "abc",
		"no thinking here": "no thinking here",
		// Unterminated: the reply was cut off mid-thought, so drop the tail.
		"before<think>never closed": "before",
	}
	for in, want := range cases {
		if got := stripThinkBlocks(in); got != want {
			t.Errorf("stripThinkBlocks(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bare", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose either side", `Here you go: {"a":1} hope that helps`, `{"a":1}`},
		{"after think block", "<think>hmm {not json}</think>\n{\"a\":1}", `{"a":1}`},
		{"nested", `{"a":{"b":2}}`, `{"a":{"b":2}}`},
		// A brace inside a string must not end the object, and a trailing "}"
		// in prose must not extend it - the old first-{ to last-} span did both.
		{"brace in string", `{"suggestion":"fix: handle } in input"} then prose}`, `{"suggestion":"fix: handle } in input"}`},
		{"escaped quote", `{"reason":"says \"done\" but isn't"}`, `{"reason":"says \"done\" but isn't"}`},
	}
	for _, c := range cases {
		got, ok := extractJSONObject(c.in)
		if !ok || got != c.want {
			t.Errorf("%s: extractJSONObject(%q) = %q, %v; want %q, true", c.name, c.in, got, ok, c.want)
		}
	}
	if _, ok := extractJSONObject("no object here"); ok {
		t.Error("expected no object")
	}
	if _, ok := extractJSONObject(`{"unclosed": 1`); ok {
		t.Error("unbalanced object should not parse")
	}
}

func TestParseVerdictEmptyContentExplainsItself(t *testing.T) {
	_, err := parseVerdict("   ")
	if err == nil {
		t.Fatal("expected an error for empty content")
	}
	for _, want := range []string{"empty content", "json_schema", "GIT_CRUX_REASONING_EFFORT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateVerdict(t *testing.T) {
	for _, ok := range []string{"accurate", "vague", "incomplete", "wrong"} {
		if err := validateVerdict(&verdict{Verdict: ok}); err != nil {
			t.Errorf("validateVerdict(%q) = %v, want nil", ok, err)
		}
	}
	if err := validateVerdict(&verdict{Verdict: "probably fine"}); err == nil {
		t.Error("expected an error for an invented verdict")
	}
}

func TestReasoningEffortOnlyWhenSet(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"accurate\",\"suggestion\":\"\",\"reason\":\"ok\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)

	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")
	if _, err := verdictCall(context.Background(), "m", "l", nil); err != nil {
		t.Fatal(err)
	}
	if _, present := bodies[0]["reasoning_effort"]; present {
		t.Error("reasoning_effort must be omitted when the env var is unset")
	}

	t.Setenv("GIT_CRUX_REASONING_EFFORT", "none")
	if _, err := verdictCall(context.Background(), "m", "l", nil); err != nil {
		t.Fatal(err)
	}
	if got := bodies[1]["reasoning_effort"]; got != "none" {
		t.Errorf("reasoning_effort = %v, want \"none\"", got)
	}
}

// The bug this fixes: a strict json_schema request answered with empty content.
func TestEmptyContentFallsBackExactlyOnce(t *testing.T) {
	var calls []bool // whether each request carried response_format
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, structured := body["response_format"]
		calls = append(calls, structured)
		if structured {
			fmt.Fprint(w, `{"choices":[{"message":{"content":""}}]}`) // the observed failure
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"<think>pondering</think>\n`+"```"+`json\n{\"verdict\":\"vague\",\"suggestion\":\"feat: add x\",\"reason\":\"generic\"}\n`+"```"+`"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")

	v, err := verdictCall(context.Background(), "m", "l", nil)
	if err != nil {
		t.Fatalf("expected the fallback to recover, got %v", err)
	}
	if v.Verdict != "vague" || v.Suggestion != "feat: add x" {
		t.Errorf("got %+v", v)
	}
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Errorf("want exactly 2 calls (structured, then unconstrained), got %v", calls)
	}
}

func TestEmptyContentRetriedOnlyOnce(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		fmt.Fprint(w, `{"choices":[{"message":{"content":""}}]}`) // empty both times
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")

	if _, err := verdictCall(context.Background(), "m", "l", nil); err == nil {
		t.Fatal("expected an error when both attempts come back empty")
	}
	if n != 2 {
		t.Errorf("made %d requests, want exactly 2", n)
	}
}

// A transport-level failure must not trigger the fallback: the retry is for
// empty content only, and genuine errors still fail open in the caller.
func TestHTTPErrorDoesNotFallBack(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)

	if _, err := verdictCall(context.Background(), "m", "l", nil); err == nil {
		t.Fatal("expected the server error to surface")
	}
	if n != 1 {
		t.Errorf("made %d requests, want 1 (no fallback on HTTP errors)", n)
	}
}

func TestUsableVerdict(t *testing.T) {
	cases := []struct {
		v    verdict
		want bool
	}{
		{verdict{Verdict: "accurate", Suggestion: ""}, true},     // legitimately has none
		{verdict{Verdict: "vague", Suggestion: "feat: x"}, true}, // the normal case
		{verdict{Verdict: "incomplete", Suggestion: ""}, false},  // the observed defect
		{verdict{Verdict: "wrong", Suggestion: "   "}, false},    // whitespace is not a suggestion
	}
	for _, c := range cases {
		if got := usableVerdict(&c.v); got != c.want {
			t.Errorf("usableVerdict(%+v) = %v, want %v", c.v, got, c.want)
		}
	}
}

// The second failure shape: the schema is satisfied but the fields are empty,
// which is what deepseek-r1-distill-qwen-7b returns under LM Studio.
func TestEmptyFieldsFallsBack(t *testing.T) {
	var calls []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, structured := body["response_format"]
		calls = append(calls, structured)
		if structured {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"reason\":\"\",\"suggestion\":\"\",\"verdict\":\"incomplete\"}"}}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"vague\",\"suggestion\":\"feat: add sum function\",\"reason\":\"generic\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")

	v, err := verdictCall(context.Background(), "m", "l", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Suggestion != "feat: add sum function" {
		t.Errorf("got %+v, want the unconstrained answer", v)
	}
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Errorf("want structured then unconstrained, got %v", calls)
	}
}

// An accurate verdict legitimately carries no suggestion; it must not retry.
func TestAccurateDoesNotFallBack(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"accurate\",\"suggestion\":\"\",\"reason\":\"describes the change\"}"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")

	v, err := verdictCall(context.Background(), "m", "l", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "accurate" || n != 1 {
		t.Errorf("verdict=%q after %d requests; want accurate after 1", v.Verdict, n)
	}
}

// LM Studio answers an oversized prompt with HTTP 200 and an empty choices
// array: no error text to match on, so the overflow has to be inferred from
// the shape of the reply or the halving retry never runs.
func TestNoChoicesCountsAsContextOverflow(t *testing.T) {
	if !isContextOverflow(errNoChoices) {
		t.Error("errNoChoices must be treated as a context overflow")
	}
	if !isContextOverflow(fmt.Errorf("wrapped: %w", errNoChoices)) {
		t.Error("a wrapped errNoChoices must still be recognised")
	}
	if isContextOverflow(errors.New("connection refused")) {
		t.Error("an unrelated error must not be treated as an overflow")
	}
}

func TestNoChoicesSurfacesAsErrNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[]}`) // exactly what LM Studio returns
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL)
	t.Setenv("GIT_CRUX_REASONING_EFFORT", "")

	_, err := chatCompletion(context.Background(), "m", "l", nil, nil)
	if !errors.Is(err, errNoChoices) {
		t.Fatalf("got %v, want errNoChoices", err)
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("the message should point at the context window, got %q", err)
	}
}

func TestLoadedContextTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// max_context_length is the model's theoretical maximum and must be
		// ignored; loaded_context_length is what the server can actually take.
		fmt.Fprint(w, `{"data":[
			{"id":"other","state":"loaded","max_context_length":131072,"loaded_context_length":4096},
			{"id":"m","state":"loaded","max_context_length":131072,"loaded_context_length":8192}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL+"/v1")

	loadedContext = contextCache{}
	if got := loadedContextTokens("m"); got != 8192 {
		t.Errorf("loadedContextTokens = %d, want 8192 (not the 131072 maximum)", got)
	}

	// contextTokens must prefer the probe over the guess from the model id...
	loadedContext = contextCache{}
	t.Setenv("GIT_CRUX_MODEL", "m")
	if got := contextTokens("m"); got != 8192 {
		t.Errorf("contextTokens = %d, want the probed 8192", got)
	}
	// ...but an explicit override still wins over both.
	loadedContext = contextCache{}
	t.Setenv("GIT_CRUX_CONTEXT", "2048")
	if got := contextTokens("m"); got != 2048 {
		t.Errorf("contextTokens = %d, want the explicit 2048", got)
	}
}

func TestLoadedContextTokensFallsBackQuietly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m","state":"not-loaded","max_context_length":131072}]}`)
	}))
	defer srv.Close()
	t.Setenv("GIT_CRUX_BASE_URL", srv.URL+"/v1")

	loadedContext = contextCache{}
	if got := loadedContextTokens("m"); got != 0 {
		t.Errorf("an unloaded model must probe to 0, got %d", got)
	}
	loadedContext = contextCache{}
	if got := loadedContextTokens("absent"); got != 0 {
		t.Errorf("an unknown model must probe to 0, got %d", got)
	}
	// The guess is then used, unchanged from before this feature existed.
	loadedContext = contextCache{}
	if got := contextTokens("microsoft/phi-4"); got != 16384 {
		t.Errorf("contextTokens = %d, want the 16384 guess", got)
	}
}

// A hosted endpoint must not be probed: /api/v0/models cannot succeed there and
// the request would only add latency to every run.
func TestOpenAIIsNotProbed(t *testing.T) {
	t.Setenv("GIT_CRUX_BASE_URL", "https://api.openai.com/v1")
	loadedContext = contextCache{}
	start := time.Now()
	if got := loadedContextTokens("gpt-4o-mini"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("took %v; api.openai.com should be skipped without a request", elapsed)
	}
}
