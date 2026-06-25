# git-crux

A small Go CLI that keeps commit messages **on point**. It compares the message
you wrote against the **actual staged diff** and, when the message is vague,
incomplete, or wrong, suggests a sharper one — anchored on your stated intent
rather than generating from scratch.

It talks to any **OpenAI-compatible** chat endpoint. By default it uses OpenAI's
`gpt-4o-mini` (reading `OPENAI_API_KEY` from the environment), which produces the
most reliable messages. To keep diffs **on your machine**, point it at a local
server instead — [LM Studio](https://lmstudio.ai), Ollama (`:11434/v1`), or
llama.cpp — by setting `GIT_CRUX_BASE_URL` and `GIT_CRUX_MODEL` (see
Configuration). Note: with the OpenAI default, each reviewed diff is sent to
OpenAI.

## Why "crux"

It gets to the crux of the change: does your message actually describe what you
did?

## Install

```sh
go install github.com/hadfielj/git-crux@latest   # puts `git-crux` on $PATH
```

Because git resolves `git crux` to a `git-crux` binary on `PATH`, you get the
`git crux` subcommand for free.

Then run a local model server. With LM Studio, start its server (default port
1234) and load a model. **Model choice matters a lot** (see Status) — a 14B-class
instruct model such as `microsoft/phi-4` is recommended.

## Use

**As an explicit command** (no setup beyond install):

```sh
git add .
git crux -m "updates my application"
#   message:  updates my application
#   verdict:  vague — message is generic and does not describe specific changes
#   suggest:  Add retry logic with exponential backoff to upload function
#   [A]ccept suggestion / [e]dit / [k]eep original?
```

**As a hook**, so plain `git commit` is handled automatically:

```sh
git crux init          # installs .git/hooks/prepare-commit-msg
git commit -m "fix"    # git-crux steps in only if the message is off-point
git commit             # no -m: git-crux pre-fills the editor with a generated message
```

To cover **every** repository at once, install globally via git's `core.hooksPath`:

```sh
git crux init --global   # installs into ~/.config/git/hooks and points core.hooksPath there
```

Note: `core.hooksPath` replaces per-repo `.git/hooks`, so any existing repo-local
hooks won't run unless moved into the shared directory.

With no `-m`, the generated message lands in your `$EDITOR` — edit and save to
accept, or empty the buffer to abort. (Merge, squash, and amend commits are left
untouched.)

**Dry run** — print the verdict as JSON without committing (handy for testing):

```sh
git crux -m "fix stuff" -dry-run
```

## Configuration

All optional; the defaults target OpenAI's `gpt-4o-mini`. **If no API key is
found** (neither `GIT_CRUX_API_KEY` nor `OPENAI_API_KEY`), git-crux falls back to
a local server (`http://localhost:1234/v1`, model `microsoft/phi-4`) instead of
erroring — so it works out of the box whether or not you have a key.

| Env var              | Default                      | Meaning                                   |
| -------------------- | ---------------------------- | ----------------------------------------- |
| `GIT_CRUX_BASE_URL`  | `https://api.openai.com/v1`  | OpenAI-compatible base URL. Set to e.g. `http://localhost:1234/v1` to run local. |
| `GIT_CRUX_MODEL`     | `gpt-4o-mini`                | Model id to request.                      |
| `GIT_CRUX_API_KEY`   | _(falls back to `OPENAI_API_KEY`)_ | Bearer token for hosted APIs; ignored by local servers.|
| `GIT_CRUX_SKIP`      | _(unset)_                    | Set to any value to disable evaluation.   |
| `GIT_CRUX_CONTEXT`   | _(auto from model)_          | Model context window in tokens; sizes the diff sent for review. |
| `GIT_CRUX_MAX_DIFF`  | _(auto from context)_        | Hard cap on diff bytes sent; overrides the context-derived budget. |

## Flags

| Flag        | Default            | Meaning                                       |
| ----------- | ------------------ | --------------------------------------------- |
| `-m`        | —                  | Commit message. Omit to have git-crux generate one. |
| `-model`    | _(resolved at runtime)_ | Model to use. Defaults to `GIT_CRUX_MODEL`, else the active server profile (`gpt-4o-mini` for OpenAI, `microsoft/phi-4` local). |
| `-no-ai`    | `false`            | Skip evaluation, commit as-is.                |
| `-dry-run`  | `false`            | Print the verdict JSON and exit.              |

## Behaviour & guardrails

- **Fails open.** If the model server is unreachable, errors, or returns
  unparseable output, the commit proceeds untouched. git-crux never blocks a
  commit.
- **Quiet when the message is good.** It only prompts on a `vague`,
  `incomplete`, or `wrong` verdict.
- **Skips non-interactive contexts.** No terminal, or `CI` set → does nothing.
- **Skips merge / squash / amend** commits.
- **Bypass** any time with `GIT_CRUX_SKIP=1` or `git commit --no-verify`.
- The hook **chains** to a pre-existing `prepare-commit-msg` hook (preserved as
  `prepare-commit-msg.local`).

## Status

v0.1 — works end to end against OpenAI and OpenAI-compatible local servers.

**Model quality is the gating factor**, especially for *generating* messages
(bare `git crux` / empty `git commit`), which needs a capable model with enough
context: `gpt-4o-mini` is reliable, while small local models (e.g. an 8K-context
`microsoft/phi-4`) tend to parrot the prompt or truncate large diffs. Verdicts
are requested at `temperature 0` for determinism. This is an MVP: the prompt is
tuned against a small scenario set, not a broad benchmark, so expect rough edges
on unusual diffs.

Obvious next steps: a curated evaluation set to measure precision/recall before
prompt changes, and large-diff chunking. (Global install via `core.hooksPath` is
done — see `git crux init --global`.)

## Build from source

```sh
make build     # -> ./git-crux
make install   # go install
```

## License

[MIT](LICENSE) © 2026 Jon Hadfield
