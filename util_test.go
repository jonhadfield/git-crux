package main

import (
	"strings"
	"testing"
)

// makeFileDiff fabricates a unified-diff chunk for one file with body of n bytes
// on a single added line (used to exercise byte-budget truncation).
func makeFileDiff(path string, n int) string {
	header := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n"
	return header + "+" + strings.Repeat("x", n) + "\n"
}

// makeFileDiffLines fabricates a chunk with `added` separate added lines (used to
// exercise per-file line counting in diffSummary).
func makeFileDiffLines(path string, added int) string {
	header := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -0,0 +1 @@\n"
	return header + strings.Repeat("+line\n", added)
}

// TestTruncateDiffKeepsEveryFile reproduces the original bug: a large early file
// (README.md sorts first) must not push other files out of the model's view.
func TestTruncateDiffKeepsEveryFile(t *testing.T) {
	diff := makeFileDiff("README.md", 8000) +
		makeFileDiff("commit.go", 800) +
		makeFileDiff("hook.go", 50) +
		makeFileDiff("llm.go", 4000)

	const max = 6000
	out := truncateDiff(diff, max)

	for _, f := range []string{"README.md", "commit.go", "hook.go", "llm.go"} {
		if !strings.Contains(out, "diff --git a/"+f) {
			t.Errorf("file %q dropped from truncated diff", f)
		}
	}
}

// TestTruncateDiffRespectsBudget keeps the output within a small margin of max
// (per-file truncation markers add a little overhead, but it must stay bounded).
func TestTruncateDiffRespectsBudget(t *testing.T) {
	diff := makeFileDiff("README.md", 8000) +
		makeFileDiff("commit.go", 8000) +
		makeFileDiff("hook.go", 8000)

	const max = 6000
	out := truncateDiff(diff, max)

	marker := "\n...(diff truncated)..."
	overhead := len(strings.Split(diff, "diff --git")) * (len(marker) + 1)
	if len(out) > max+overhead {
		t.Errorf("output %d bytes exceeds budget %d (+overhead %d)", len(out), max, overhead)
	}
}

// TestDiffSummaryOrdersByMagnitude verifies the file map leads with the largest
// change, not the alphabetically-first file — the anchoring the model fell for.
func TestDiffSummaryOrdersByMagnitude(t *testing.T) {
	diff := makeFileDiffLines("README.md", 3) + // sorts first, small change
		makeFileDiffLines("llm.go", 50) // sorts last, large change

	summary := diffSummary(diff)
	readmeAt := strings.Index(summary, "README.md")
	llmAt := strings.Index(summary, "llm.go")
	if readmeAt < 0 || llmAt < 0 {
		t.Fatalf("summary missing a file:\n%s", summary)
	}
	if llmAt > readmeAt {
		t.Errorf("expected llm.go (larger) before README.md:\n%s", summary)
	}
}

// TestDiffSummarySingleFileEmpty returns no summary when only one file changed.
func TestDiffSummarySingleFileEmpty(t *testing.T) {
	if s := diffSummary(makeFileDiffLines("only.go", 10)); s != "" {
		t.Errorf("expected empty summary for single file, got:\n%s", s)
	}
}

// TestTruncateDiffSmallDiffUnchanged leaves a diff under budget untouched.
func TestTruncateDiffSmallDiffUnchanged(t *testing.T) {
	diff := makeFileDiff("a.go", 10) + makeFileDiff("b.go", 10)
	if out := truncateDiff(diff, 6000); out != diff {
		t.Errorf("small diff was modified:\n%s", out)
	}
}

func TestStripComments(t *testing.T) {
	in := "Add retry logic\n\nbody line\n# Please enter the commit message for your changes.\n#\n# On branch main\n"
	want := "Add retry logic\n\nbody line"
	if got := stripComments(in); got != want {
		t.Errorf("stripComments:\n%q\nwant:\n%q", got, want)
	}
}

// TestStripCommentsAllComments returns "" when nothing but git's template remains,
// which is what triggers the generate-from-scratch path in the hook.
func TestStripCommentsAllComments(t *testing.T) {
	in := "# Please enter the commit message for your changes.\n#\n# Changes to be committed:\n"
	if got := stripComments(in); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestIndent(t *testing.T) {
	in := "Initial commit: x\n\n- one\n- two\n"
	want := "    Initial commit: x\n\n    - one\n    - two"
	if got := indent(in, "    "); got != want {
		t.Errorf("indent:\n%q\nwant:\n%q", got, want)
	}
}
