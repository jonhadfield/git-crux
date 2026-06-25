package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Defaults target OpenAI's gpt-4o-mini, which reliably produces grounded
	// multi-line messages. The API key is read from OPENAI_API_KEY (see apiKey).
	// To run fully local instead, set GIT_CRUX_BASE_URL to a local
	// OpenAI-compatible server (e.g. LM Studio at http://localhost:1234/v1) and
	// GIT_CRUX_MODEL to a model it serves.
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "gpt-4o-mini"

	// Local fallback profile. When no API key is available, the OpenAI default
	// would only 401, so we fall back to a local LM Studio server instead.
	localBaseURL = "http://localhost:1234/v1"
	localModel   = "microsoft/phi-4"

	// Diff-budget tuning. The diff sent to the model is sized from the model's
	// context window rather than a fixed cap, so a small model is not overflowed
	// and a large one is not needlessly starved.
	defaultContextTokens = 8192  // assumed window for unrecognised models
	reservedTokens       = 1500  // system prompt + message wrapper + JSON reply
	charsPerToken        = 3     // conservative bytes/token, so we under-fill
	minDiffChars         = 4000  // floor: always show a useful amount
	maxDiffCharsCeil     = 32000 // ceiling: bound local-inference latency & "lost in the middle"
)

// knownContext maps a lower-cased substring of a model id to its context window
// in tokens. The first match wins, so more specific keys come first. Sizes are
// the documented windows for each family; unknown models fall back to
// defaultContextTokens. Override directly with GIT_CRUX_CONTEXT when a model is
// loaded with a non-standard window (common in LM Studio).
var knownContext = []struct {
	match  string
	tokens int
}{
	{"phi-4", 16384},
	{"phi-3", 4096},
	{"qwen2.5", 32768},
	{"qwen", 32768},
	{"llama-3.1", 131072},
	{"llama3.1", 131072},
	{"llama-3", 8192},
	{"llama3", 8192},
	{"mixtral", 32768},
	{"mistral", 32768},
	{"gemma2", 8192},
	{"gemma", 8192},
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"gpt-4", 8192},
	{"gpt-3.5", 16385},
}

// contextTokens returns the context window (in tokens) for model: an explicit
// GIT_CRUX_CONTEXT wins, then the knownContext table, then the default.
func contextTokens(model string) int {
	if n, ok := envInt("GIT_CRUX_CONTEXT"); ok {
		return n
	}
	m := strings.ToLower(model)
	for _, e := range knownContext {
		if strings.Contains(m, e.match) {
			return e.tokens
		}
	}
	return defaultContextTokens
}

// diffBudget returns how many diff bytes to send for model. GIT_CRUX_MAX_DIFF
// overrides everything; otherwise the budget scales with the model's context
// window, clamped to a sane [floor, ceiling].
func diffBudget(model string) int {
	if n, ok := envInt("GIT_CRUX_MAX_DIFF"); ok {
		return n
	}
	budget := (contextTokens(model) - reservedTokens) * charsPerToken
	if budget < minDiffChars {
		return minDiffChars
	}
	if budget > maxDiffCharsCeil {
		return maxDiffCharsCeil
	}
	return budget
}

// envInt reads a positive integer from an environment variable. ok is false
// when the variable is unset, empty, non-numeric, or not positive.
func envInt(name string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// verdict is the structured judgement returned by the model.
type verdict struct {
	// Verdict is one of: accurate, vague, incomplete, wrong.
	Verdict string `json:"verdict"`
	// Suggestion is an improved one-line message; empty when accurate.
	Suggestion string `json:"suggestion"`
	// Reason is a short phrase explaining the verdict.
	Reason string `json:"reason"`
}

func baseURL() string {
	if v := os.Getenv("GIT_CRUX_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if apiKey() == "" {
		return localBaseURL // no key → the OpenAI default would only 401
	}
	return defaultBaseURL
}

func modelName() string {
	if v := os.Getenv("GIT_CRUX_MODEL"); v != "" {
		return v
	}
	// Default the model to match the server we end up talking to.
	if strings.HasPrefix(baseURL(), defaultBaseURL) {
		return defaultModel
	}
	return localModel
}

// Commit-message styles. conventional is the default: git-crux follows the
// Conventional Commits standard (type-prefixed subjects) unless GIT_CRUX_STYLE
// or -style asks for the plain imperative style instead.
const (
	stylePlain        = "plain"
	styleConventional = "conventional"
	defaultStyle      = styleConventional
)

// commitStyle resolves the active style: an explicit value (from the -style
// flag) wins, then GIT_CRUX_STYLE, then the default. An unrecognised value falls
// back to the default with a warning so a typo never silently changes behavior.
func commitStyle(flag string) string {
	v := strings.TrimSpace(flag)
	if v == "" {
		v = strings.TrimSpace(os.Getenv("GIT_CRUX_STYLE"))
	}
	switch strings.ToLower(v) {
	case "":
		return defaultStyle
	case stylePlain:
		return stylePlain
	case styleConventional:
		return styleConventional
	default:
		fmt.Fprintf(os.Stderr, "git-crux: unknown style %q; using %q\n", v, defaultStyle)
		return defaultStyle
	}
}

// apiKey is optional; LM Studio ignores it, hosted OpenAI requires it. We fall
// back to the conventional OPENAI_API_KEY so pointing git-crux at OpenAI needs
// only a base URL and model, not a duplicated key.
func apiKey() string {
	if k := os.Getenv("GIT_CRUX_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("OPENAI_API_KEY")
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// verdictResponseFormat pins the model output to our schema. This is the
// OpenAI/LM Studio structured-outputs format; the enum guarantees a valid
// verdict value so we never have to defend against typos like "Vague.".
var verdictResponseFormat = map[string]any{
	"type": "json_schema",
	"json_schema": map[string]any{
		"name":   "verdict",
		"strict": true,
		"schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"verdict":    map[string]any{"type": "string", "enum": []string{"accurate", "vague", "incomplete", "wrong"}},
				"suggestion": map[string]any{"type": "string", "description": "Complete commit message: a subject line, then (for non-trivial changes) a blank line and a '- ' bulleted body. Empty string when the verdict is accurate."},
				"reason":     map[string]any{"type": "string"},
			},
			"required": []string{"verdict", "suggestion", "reason"},
		},
	},
}

// evaluate asks the model whether message accurately describes diff. It starts
// from the model's estimated diff budget and, if the prompt overflows the
// model's ACTUAL loaded context, halves the diff and retries down to a floor.
// This discovers the real limit empirically rather than trusting a guess, which
// matters because a model can be loaded with a smaller window than its nominal
// maximum (e.g. phi-4 loaded at 8K instead of 16K).
func evaluate(ctx context.Context, message, diff, model, style string) (*verdict, error) {
	budget := diffBudget(model)
	for {
		v, err := evaluateWithBudget(ctx, message, diff, model, style, budget)
		if err == nil || !isContextOverflow(err) || budget <= minDiffChars {
			return v, err
		}
		next := budget / 2
		if next < minDiffChars {
			next = minDiffChars
		}
		fmt.Fprintf(os.Stderr, "git-crux: prompt exceeded model context; retrying with a smaller diff (%d→%d bytes)\n", budget, next)
		budget = next
	}
}

// isContextOverflow reports whether err is the model server rejecting the prompt
// for exceeding its context window, as opposed to any other failure.
func isContextOverflow(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "context length") ||
		strings.Contains(s, "context window") ||
		strings.Contains(s, "n_ctx") ||
		strings.Contains(s, "tokens to keep") ||
		strings.Contains(s, "maximum context") ||
		strings.Contains(s, "too many tokens")
}

// evaluateWithBudget performs one /chat/completions call, sending at most budget
// bytes of diff.
func evaluateWithBudget(ctx context.Context, message, diff, model, style string, budget int) (*verdict, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []chatMessage{
			{Role: "system", Content: systemPrompt(style)},
			{Role: "user", Content: userPrompt(message, diffSummary(diff), truncateDiff(diff, budget))},
		},
		"temperature":     0,
		"stream":          false,
		"response_format": verdictResponseFormat,
	})
	if err != nil {
		return nil, err
	}

	// Generous timeout: the first request may trigger a model load.
	client := &http.Client{Timeout: 90 * time.Second}

	// Show a spinner while we wait. It stops on every return path (including the
	// context-overflow error), so the outer retry message in evaluate prints to a
	// clean line. No-op in non-interactive contexts.
	sp := startSpinner("asking " + model)
	defer sp.Stop()

	resp, err := doWithRetry(ctx, client, baseURL()+"/chat/completions", reqBody)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("calling model server at %s (is LM Studio running?): %w", baseURL(), err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("decoding server response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("model returned no choices")
	}

	v, err := parseVerdict(cr.Choices[0].Message.Content)
	if err != nil {
		return nil, err
	}
	v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
	v.Suggestion = strings.TrimSpace(v.Suggestion)
	return v, nil
}

// doWithRetry POSTs body to url, retrying once after a short pause on a transient
// network error (connection refused/reset, dial timeout). It does NOT retry once
// the server has answered — a non-2xx status comes back to the caller on the
// first try — nor when ctx is cancelled, so Ctrl-C aborts immediately. A fresh
// request is built per attempt because the body reader is consumed each time.
func doWithRetry(ctx context.Context, client *http.Client, url string, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if k := apiKey(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, err // cancelled or timed out: do not retry
		}
		lastErr = err
	}
	return nil, lastErr
}

// parseVerdict unmarshals the model's content, tolerating models that wrap the
// JSON object in surrounding prose or markdown fences.
func parseVerdict(content string) (*verdict, error) {
	content = strings.TrimSpace(content)
	var v verdict
	if err := json.Unmarshal([]byte(content), &v); err == nil {
		return &v, nil
	}
	if start := strings.IndexByte(content, '{'); start >= 0 {
		if end := strings.LastIndexByte(content, '}'); end > start {
			if err := json.Unmarshal([]byte(content[start:end+1]), &v); err == nil {
				return &v, nil
			}
		}
	}
	return nil, fmt.Errorf("could not parse model output as JSON: %q", truncate(content, 200))
}

// systemPrompt assembles the model instructions for the active commit style.
// A shared head (verdict logic, multi-file handling) is followed by
// style-specific format rules and examples, so prompt tuning of the common
// parts happens in one place.
func systemPrompt(style string) string {
	if style == styleConventional {
		return promptHead + promptFormatConventional + promptAbsolute + examplesConventional
	}
	return promptHead + promptFormatPlain + promptAbsolute + examplesPlain
}

const promptHead = `You review git commit messages. You are given a developer's commit message and the actual staged diff. Judge whether the message describes the change well enough to be useful in the project history.

Choose the verdict in THIS PRIORITY ORDER:
1. "wrong": the message describes something that does NOT appear in the diff; the subject is mismatched (e.g. the message mentions the README but the diff only touches code).
2. "vague": the message is generic boilerplate that names nothing specific about THIS change and could apply to almost any commit (e.g. "update", "fix stuff", "changes", "wip", "updates my application").
3. "incomplete": the message accurately describes its subject, but the diff ALSO makes a second change that is UNRELATED to that subject and would surprise a reader (e.g. the message says "add debounce" but the diff also bumps an API version). Before choosing this, you must be able to name the specific unrelated change.
4. "accurate": the message conveys the main intent of the change. A high-level but correct message is accurate even if it does not restate the mechanism, function names, parameters, or values.

CRITICAL RULES:
- If the message already names the change the diff makes, choose "accurate". NEVER report a part as "omitted" or "missing" if the message already mentions it (even loosely). For example, if the message says "retry with exponential backoff" and the diff adds a retry loop with backoff, that is "accurate", not "incomplete".
- Do NOT choose "incomplete" merely because the message omits mechanisms, parameters, defaults, imports, or details that belong to the change it already describes. "incomplete" requires a genuinely SEPARATE, unrelated change.
- Be reluctant to flag. When in doubt between "accurate" and "incomplete", choose "accurate".

Multi-file commits:
- When the diff touches several files, judge and summarize the change AS A WHOLE, not whichever file appears first. The diff is ordered by filename, so a documentation file may lead even when the substantive work is elsewhere.
- A "Files changed" list (largest change first) precedes the diff. Use it to locate where the substance is. The SUBJECT line should reflect the dominant change; prefer functional code changes over documentation, comments, or formatting when ranking. The BODY bullets then cover the other notable changes so nothing significant is dropped.
- The subject describes the overall intent of the commit, not one incidental file.
`

const promptFormatPlain = `
Output rules:
- Respond with ONLY the JSON object, no prose or markdown fences.
- "suggestion": when the verdict is NOT "accurate", a COMPLETE commit message describing THIS diff; otherwise "".
  Format: an imperative subject line of 72 characters or fewer summarizing the whole change. When the diff touches several files or makes more than one notable change, follow the subject with a blank line and "- " bullet lines, one per notable change, ordered by importance.
  Rules for the suggestion:
  * Always include the subject line. Add the blank line and "- " bullets ONLY when the diff touches several files or makes more than one notable change; a small single-purpose change is just the subject.
  * Order bullets by importance; use the "Files changed" map so no significant area is missed; describe behavior or areas, not bare filenames.
  * PRESERVE the developer's stated intent — their message is a seed. If it frames the commit (e.g. "initial." means this IS the initial commit), keep that meaning in the subject and expand in the body.
  * If the developer's message is EMPTY, there is nothing to preserve: choose verdict "vague" and write a complete commit message for the diff from scratch.
- "reason": a concrete phrase of 12 words or fewer explaining WHY, referring to the actual change or mismatch. NEVER simply repeat the verdict word.
`

const promptFormatConventional = `
This project follows the Conventional Commits standard (https://www.conventionalcommits.org). Every commit subject MUST begin with a type prefix: "<type>: " or "<type>(<scope>): ".

Allowed types, and when each applies:
- feat: a new user-facing capability or behavior.
- fix: corrects broken or incorrect behavior.
- perf: improves performance without changing behavior.
- refactor: restructures code without changing behavior or adding a feature.
- docs: documentation only.
- test: adds or corrects tests only.
- build: build system, packaging, or dependency changes.
- ci: CI configuration or scripts (e.g. GitHub Actions, .github/workflows).
- style: formatting/whitespace only, no change in code meaning.
- chore: maintenance that fits no other type.
- revert: reverts a previous commit.
Choose the type that matches the DOMINANT change in the diff (use the "Files changed" map). If the change is a breaking API change, append "!" after the type or scope (e.g. "feat!:" or "feat(api)!:").

How the prefix affects the verdict:
- A message is "accurate" ONLY IF it both (a) conveys the main intent AND (b) already begins with a valid type prefix appropriate to the diff.
- If the wording describes the change well but the type prefix is MISSING, choose "vague"; if a prefix is present but names the WRONG type for the diff, choose "wrong". State the prefix problem in "reason".

Output rules:
- Respond with ONLY the JSON object, no prose or markdown fences.
- "suggestion": when the verdict is NOT "accurate", a COMPLETE Conventional Commits message for THIS diff; otherwise "".
  Format: "<type>(<optional scope>): <imperative description>" as the subject line, 72 characters or fewer. When the diff touches several files or makes more than one notable change, follow the subject with a blank line and "- " bullet lines, one per notable change, ordered by importance.
  Rules for the suggestion:
  * Always begin with a valid type prefix. Add the blank line and "- " bullets ONLY when the diff touches several files or makes more than one notable change; a small single-purpose change is just the prefixed subject.
  * Order bullets by importance; use the "Files changed" map so no significant area is missed; describe behavior or areas, not bare filenames.
  * PRESERVE the developer's stated intent — their message is a seed. If their message already implies a type, honor it unless the diff contradicts it. If it frames the commit (e.g. "initial." means this IS the initial commit), keep that meaning after the prefix.
  * If the developer's message is EMPTY, there is nothing to preserve: choose verdict "vague" and write a complete Conventional Commits message for the diff from scratch.
- "reason": a concrete phrase of 12 words or fewer explaining WHY, referring to the actual change, mismatch, or missing/incorrect type. NEVER simply repeat the verdict word.
`

const promptAbsolute = `
ABSOLUTE RULE: Describe ONLY what THIS diff changes. Do NOT copy or adapt wording from these instructions or the examples below. Words like "login", "retry", "queue", "worker", "Redis", "search", "debounce", "API" must NOT appear in your output unless they genuinely appear in the diff. If you cannot ground a bullet in the diff, omit it.
`

const examplesPlain = `
Examples (these illustrate the verdict choice and the JSON shape ONLY — never reuse their wording):
- diff adds input validation to a login handler, message "stuff":
{"verdict":"vague","suggestion":"Validate email and password in login handler","reason":"message names no part of the change"}
- diff adds a retry loop with exponential backoff to upload(), message "add retry with exponential backoff to upload":
{"verdict":"accurate","suggestion":"","reason":"message matches the change the diff makes"}
- diff adds a worker entry point, a Redis queue client, and a config loader across several new files, message "initial.":
{"verdict":"vague","suggestion":"Initial commit: scaffold the job queue worker\n\n- Add the worker entry point with graceful shutdown\n- Add a Redis-backed queue client\n- Load worker settings from environment variables","reason":"message names nothing about the change"}
- diff adds debounce to a search handler AND bumps the API base URL from v1 to v2, message "add debounce to search":
{"verdict":"incomplete","suggestion":"Debounce search input and upgrade API base URL to v2","reason":"unrelated API version bump not mentioned"}`

const examplesConventional = `
Examples (these illustrate the verdict choice and the JSON shape ONLY — never reuse their wording):
- diff adds input validation to a login handler, message "stuff":
{"verdict":"vague","suggestion":"feat: validate email and password in login handler","reason":"generic message with no type prefix"}
- diff adds a retry loop with exponential backoff to upload(), message "feat: add retry with exponential backoff to upload":
{"verdict":"accurate","suggestion":"","reason":"valid feat prefix matching the change"}
- diff corrects an off-by-one in pagination bounds, message "correct pagination off-by-one":
{"verdict":"vague","suggestion":"fix: correct off-by-one in pagination bounds","reason":"accurate wording but missing fix prefix"}
- diff only edits the CI workflow file, message "feat: update build":
{"verdict":"wrong","suggestion":"ci: update GitHub Actions workflow","reason":"diff is CI config, not a feature"}
- diff adds a worker entry point, a Redis queue client, and a config loader across several new files, message "initial.":
{"verdict":"vague","suggestion":"feat: scaffold the job queue worker\n\n- Add the worker entry point with graceful shutdown\n- Add a Redis-backed queue client\n- Load worker settings from environment variables","reason":"message names nothing about the change"}`

func userPrompt(message, summary, diff string) string {
	var b strings.Builder
	b.WriteString("Commit message:\n")
	b.WriteString(message)
	if summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	b.WriteString("\nStaged diff:\n")
	b.WriteString(diff)
	return b.String()
}
