package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHookIntoRefreshesStaleShim(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "prepare-commit-msg")
	stale := "#!/bin/sh\n# old git-crux shim\nexec git-crux hook prepare-commit-msg \"$@\"\n"
	if err := os.WriteFile(hookPath, []byte(stale), 0o755); err != nil {
		t.Fatal(err)
	}

	path, fresh, err := installHookInto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != hookPath {
		t.Errorf("path = %q, want %q", path, hookPath)
	}
	if !fresh {
		t.Fatal("expected fresh=true when rewriting a stale git-crux shim")
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hookShim {
		t.Fatalf("hook not refreshed:\ngot:\n%s\nwant:\n%s", got, hookShim)
	}
	if _, err := os.Stat(hookPath + ".local"); err == nil {
		t.Fatal("refreshing a git-crux shim must not create prepare-commit-msg.local")
	}
}

func TestInstallHookIntoKeepsCurrentShim(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(hookShim), 0o755); err != nil {
		t.Fatal(err)
	}

	_, fresh, err := installHookInto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("expected fresh=false when the shim is already current")
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hookShim {
		t.Fatalf("current shim was rewritten unexpectedly")
	}
}

func TestWriteMessagePreservesGitComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	raw := "old subject\n\n# Please enter the commit message\n# On branch main\n"
	want := "feat: add a logger\n# Please enter the commit message\n# On branch main\n"
	if err := writeMessage(path, "feat: add a logger", raw); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRunHookSkipsMergeSquashAmend(t *testing.T) {
	for _, source := range []string{"merge", "squash", "commit"} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			original := "do not touch me\n# comment\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := runHook(context.Background(), []string{"prepare-commit-msg", path, source}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("source %q rewrote the message file:\ngot %q\nwant %q", source, got, original)
			}
		})
	}
}

func TestRunHookSkipsWhenGIT_CRUX_SKIPSet(t *testing.T) {
	t.Setenv("GIT_CRUX_SKIP", "1")
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	original := "keep me\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runHook(context.Background(), []string{"prepare-commit-msg", path, "message"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("GIT_CRUX_SKIP still rewrote the message: %q", got)
	}
}

func TestInstallHookIntoBacksUpThirdPartyHook(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "prepare-commit-msg")
	thirdParty := "#!/bin/sh\necho third-party\n"
	if err := os.WriteFile(hookPath, []byte(thirdParty), 0o755); err != nil {
		t.Fatal(err)
	}

	_, fresh, err := installHookInto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("expected fresh=true when replacing a third-party hook")
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hookShim {
		t.Fatalf("shim not installed, got %q", got)
	}
	backup, err := os.ReadFile(hookPath + ".local")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != thirdParty {
		t.Fatalf("backup = %q, want original third-party hook", backup)
	}
	if !strings.Contains(hookShim, "git-crux") {
		t.Fatal("sanity: hookShim must mention git-crux")
	}
}
