.PHONY: check test spec lint build build-linux e2e e2e-publish e2e-sync

check: test spec

test:
	go test ./...

spec:
	python3 scripts/validate_spec.py

lint:
	npx -y @redocly/cli@latest lint spec/openapi.yaml

build:
	go build -o bin/uplink ./cmd/uplink

# Static Linux binary for servers. Zone data is embedded because usage
# summaries use a named time zone and minimal images have no zoneinfo.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -trimpath -o bin/uplink-linux-amd64 ./cmd/uplink

# make e2e NEWAPI=/path/to/new-api-binary [WORK=/tmp/uplink-e2e]
WORK ?= /tmp/uplink-e2e
e2e: e2e-publish e2e-sync

e2e-publish: build
	python3 e2e/newapi/run.py --newapi $(NEWAPI) --uplink $(CURDIR)/bin/uplink --work $(WORK)/publish
	python3 scripts/validate_spec.py --responses $(WORK)/publish/responses

# Two New API instances: ours upstream, a reseller's downstream (about 2 minutes).
e2e-sync: build
	python3 e2e/newapi/sync_run.py --newapi $(NEWAPI) --uplink $(CURDIR)/bin/uplink --work $(WORK)/sync
