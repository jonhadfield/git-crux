BINARY=git-crux

build:
	go build -o $(BINARY) .

install:
	go install .

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

# Score the model against the curated evaluation set in testdata/eval.
# Requires model env (e.g. OPENAI_API_KEY, or GIT_CRUX_BASE_URL for a local server).
# Set GIT_CRUX_EVAL_MIN=0.8 to fail below that verdict accuracy.
# -timeout 0: a local model can take over a minute per case, which would trip
# go test's default 10-minute limit. Each request is still capped by
# GIT_CRUX_TIMEOUT, so the run cannot hang.
eval:
	GIT_CRUX_EVAL=1 go test -timeout 0 -run '^TestEval$$' -v .

.PHONY: build install fmt vet test eval
