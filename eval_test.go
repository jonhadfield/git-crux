package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The evaluation set lives in testdata/eval/<case>/: a commit message
// (message.txt, possibly empty), the staged diff (diff.patch), and the expected
// outcome (want.json: {verdict, style, notes}). TestEvalCorpusValid checks the
// corpus is well-formed on every run; TestEval scores the model against it and is
// skipped unless GIT_CRUX_EVAL is set, since it needs a live model.

type evalCase struct {
	name    string
	message string
	diff    string
	want    string // expected verdict
	style   string // resolved style (defaults to the project default)
	notes   string
}

func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	root := filepath.Join("testdata", "eval")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}

	var cases []evalCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		msg, err := os.ReadFile(filepath.Join(dir, "message.txt"))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		diff, err := os.ReadFile(filepath.Join(dir, "diff.patch"))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		wantRaw, err := os.ReadFile(filepath.Join(dir, "want.json"))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		var w struct {
			Verdict string `json:"verdict"`
			Style   string `json:"style"`
			Notes   string `json:"notes"`
		}
		if err := json.Unmarshal(wantRaw, &w); err != nil {
			t.Fatalf("%s want.json: %v", e.Name(), err)
		}
		style := w.Style
		if style == "" {
			style = defaultStyle
		}
		cases = append(cases, evalCase{
			name:    e.Name(),
			message: strings.TrimRight(string(msg), "\n"),
			diff:    string(diff),
			want:    w.Verdict,
			style:   style,
			notes:   w.Notes,
		})
	}
	slices.SortFunc(cases, func(a, b evalCase) int { return strings.Compare(a.name, b.name) })
	return cases
}

var validVerdicts = map[string]bool{"accurate": true, "vague": true, "incomplete": true, "wrong": true}

// TestEvalCorpusValid runs without a model: it guards the corpus against rot —
// bad verdict labels, unknown styles, empty diffs, or an "accurate" Conventional
// Commits case whose message lacks a type prefix (which would be unscoreable).
func TestEvalCorpusValid(t *testing.T) {
	cases := loadEvalCases(t)
	if len(cases) == 0 {
		t.Fatal("no eval cases found under testdata/eval")
	}
	for _, c := range cases {
		if !validVerdicts[c.want] {
			t.Errorf("%s: invalid want verdict %q", c.name, c.want)
		}
		if c.style != stylePlain && c.style != styleConventional {
			t.Errorf("%s: invalid style %q", c.name, c.style)
		}
		if strings.TrimSpace(c.diff) == "" {
			t.Errorf("%s: empty diff", c.name)
		}
		if c.want == "accurate" && c.style == styleConventional && !hasConventionalPrefix(c.message) {
			t.Errorf("%s: accurate+conventional case needs a valid type prefix, got %q", c.name, c.message)
		}
	}
}

// suggestionProblems reports generate-quality failures for one eval case: a
// missing suggestion when the verdict is off-point, or a Conventional Commits
// suggestion with no type prefix.
func suggestionProblems(c evalCase, suggestion string) []string {
	sugg := strings.TrimSpace(suggestion)
	var problems []string
	if c.want != "accurate" && sugg == "" {
		problems = append(problems, "missing suggestion")
	}
	if c.style == styleConventional && sugg != "" && !hasConventionalPrefix(sugg) {
		problems = append(problems, "suggestion lacks Conventional Commits prefix")
	}
	return problems
}

func TestSuggestionProblems(t *testing.T) {
	plain := evalCase{name: "plain", want: "vague", style: stylePlain}
	conv := evalCase{name: "conv", want: "vague", style: styleConventional}
	ok := evalCase{name: "ok", want: "accurate", style: styleConventional}

	if p := suggestionProblems(plain, ""); len(p) != 1 || p[0] != "missing suggestion" {
		t.Errorf("empty plain generate: got %v", p)
	}
	if p := suggestionProblems(plain, "Add a prefixed logger"); len(p) != 0 {
		t.Errorf("plain suggestion: got %v", p)
	}
	if p := suggestionProblems(conv, ""); len(p) != 1 || p[0] != "missing suggestion" {
		t.Errorf("empty conventional generate: got %v", p)
	}
	if p := suggestionProblems(conv, "Add a prefixed logger"); len(p) != 1 || p[0] != "suggestion lacks Conventional Commits prefix" {
		t.Errorf("unprefixed conventional: got %v", p)
	}
	if p := suggestionProblems(conv, "feat: add a prefixed logger"); len(p) != 0 {
		t.Errorf("prefixed conventional: got %v", p)
	}
	if p := suggestionProblems(ok, ""); len(p) != 0 {
		t.Errorf("accurate needs no suggestion: got %v", p)
	}
}

// TestEval scores the configured model against the corpus and prints a per-case
// table and a confusion matrix. It is skipped unless GIT_CRUX_EVAL is set, since
// it makes real model calls. Set GIT_CRUX_EVAL_MIN to a fraction (e.g. 0.8) to
// fail when verdict accuracy drops below that floor — useful as a gate before
// changing the prompt.
//
//	GIT_CRUX_EVAL=1 go test -run '^TestEval$' -v
func TestEval(t *testing.T) {
	if os.Getenv("GIT_CRUX_EVAL") == "" {
		t.Skip("set GIT_CRUX_EVAL=1 to run the evaluation set")
	}
	// TestMain stubs auto-fm so unit tests never spawn `fm serve`. Re-enable it
	// here so `make eval` on macOS 27 uses the on-device default.
	prev := ensureAppleFM
	ensureAppleFM = ensureAppleFMOnce
	t.Cleanup(func() { ensureAppleFM = prev })

	cases := loadEvalCases(t)
	model := modelName()
	ctx := context.Background()

	order := []string{"accurate", "vague", "incomplete", "wrong"}
	confusion := map[string]map[string]int{}
	for _, v := range order {
		confusion[v] = map[string]int{}
	}

	var correct, suggGot, suggWant int
	for _, c := range cases {
		v, err := evaluate(ctx, c.message, c.diff, model, c.style)
		if err != nil {
			t.Fatalf("%s: evaluate error: %v", c.name, err)
		}
		confusion[c.want][v.Verdict]++
		status := "FAIL"
		if v.Verdict == c.want {
			correct++
			status = "pass"
		}
		sugg := strings.TrimSpace(v.Suggestion)
		if c.want != "accurate" {
			suggWant++
			if sugg != "" {
				suggGot++
			}
		}
		if problems := suggestionProblems(c, sugg); len(problems) > 0 {
			t.Errorf("%s: %s", c.name, strings.Join(problems, "; "))
			status = "FAIL"
		}
		t.Logf("%-28s want=%-10s got=%-10s %s | %s", c.name, c.want, v.Verdict, status, v.Reason)
	}

	acc := float64(correct) / float64(len(cases))
	t.Logf("")
	t.Logf("model: %s", model)
	t.Logf("verdict accuracy: %d/%d = %.0f%%", correct, len(cases), acc*100)
	t.Logf("suggestion present when expected: %d/%d", suggGot, suggWant)
	t.Logf("confusion (rows = expected, cols = predicted):")
	header := fmt.Sprintf("%-16s", "")
	for _, g := range order {
		header += fmt.Sprintf("%-12s", g)
	}
	t.Logf("%s", header)
	for _, w := range order {
		row := fmt.Sprintf("%-16s", w)
		for _, g := range order {
			row += fmt.Sprintf("%-12d", confusion[w][g])
		}
		t.Logf("%s", row)
	}

	if minStr := os.Getenv("GIT_CRUX_EVAL_MIN"); minStr != "" {
		min, err := strconv.ParseFloat(minStr, 64)
		if err != nil {
			t.Fatalf("GIT_CRUX_EVAL_MIN is not a number: %v", err)
		}
		if acc < min {
			t.Errorf("verdict accuracy %.2f is below the %.2f threshold", acc, min)
		}
	}
}
