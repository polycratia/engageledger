GO ?= go
EXAMPLE = example/engagement.jsonl

.PHONY: test example demo

test:
	$(GO) test ./...

example:
	$(GO) run ./cmd/engageledger seed $(EXAMPLE)

demo: example
	$(GO) run ./cmd/engageledger verify $(EXAMPLE)
	$(GO) run ./cmd/engageledger fold $(EXAMPLE)
	$(GO) run ./cmd/engageledger clock $(EXAMPLE)
