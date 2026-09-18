APP := prometheus-universal-exporter

# Must be a release built with at least the Go the build uses; an older one
# panics on standard-library sources from a newer toolchain. Kept in step with
# .github/workflows/ci.yml by a test.
GOLANGCI_LINT_VERSION := v2.13.2

.PHONY: build test test-external vet fmt fmt-check lint lint-install helm-test
build:
	go build ./...

test:
	go test ./...
	go test -race ./...

# Opt-in: probes real third-party endpoints, so it is deliberately not part of
# `make ci`. See docs/DEVELOPMENT.md.
test-external:
	EXTERNAL_E2E=1 go test -run TestExternal -v ./...

vet:
	go vet ./...

# The whole tree, not just the root package: tools/ holds the dependency
# resolver and is covered by the same gates.
fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed for:"; echo "$$unformatted"; echo "run: make fmt"; \
		exit 1; \
	fi

lint:
	golangci-lint run

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

helm-test:
	helm lint charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"service-targets","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"pod-targets","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set server.listenAddress=0.0.0.0:9115 --set server.pythonPath=/usr/bin/python3.11
	helm template test charts/prometheus-universal-exporter --set otlpTargets.enabled=true --set-file otlpTargets.data=targets.example.yaml --set-file 'config.data.config\.yaml=config.otlp.example.yaml'
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"a","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"},{"name":"b","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]' | python3 tools/check-manifests.py
	@# The same rejections CI checks, so a local run means a CI run.
	@for address in ':http' '9115' '0.0.0.0' ':0'; do \
		if helm template test charts/prometheus-universal-exporter --set "server.listenAddress=$$address" >/dev/null 2>&1; then \
			echo "helm template accepted the invalid server.listenAddress '$$address'" >&2; \
			exit 1; \
		fi; \
	done
	@if helm template test charts/prometheus-universal-exporter --set otlpTargets.enabled=true --set-file otlpTargets.data=targets.example.yaml --set-file 'config.data.config\.yaml=config.example.yaml' >/dev/null 2>&1; then \
		echo "helm template accepted scheduled targets while OTLP export is disabled" >&2; \
		exit 1; \
	fi

ci: fmt-check lint test vet build helm-test
