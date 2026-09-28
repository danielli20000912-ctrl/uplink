.PHONY: check test spec lint

check: test spec

test:
	go test ./...

spec:
	python3 scripts/validate_spec.py

lint:
	npx -y @redocly/cli@latest lint spec/openapi.yaml
