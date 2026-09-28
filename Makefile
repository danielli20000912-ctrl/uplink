.PHONY: check test spec lint build e2e

check: test spec

test:
	go test ./...

spec:
	python3 scripts/validate_spec.py

lint:
	npx -y @redocly/cli@latest lint spec/openapi.yaml

build:
	go build -o bin/uplink ./cmd/uplink

# make e2e NEWAPI=/path/to/new-api-binary [WORK=/tmp/uplink-e2e]
WORK ?= /tmp/uplink-e2e
e2e: build
	python3 e2e/newapi/run.py --newapi $(NEWAPI) --uplink $(CURDIR)/bin/uplink --work $(WORK)
	python3 scripts/validate_spec.py --responses $(WORK)/responses
