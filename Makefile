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
	helm template test charts/prometheus-universal-exporter --set server.expandEnv=true --set-json 'env=[{"name":"DEMO_TARGET","value":"http://api.internal:8080"}]' --set-json 'envFrom=[{"secretRef":{"name":"exporter-secrets"}}]'
	helm template test charts/prometheus-universal-exporter --set otlpTargets.enabled=true --set-file otlpTargets.data=targets.example.yaml --set-file 'config.data.config\.yaml=config.otlp.example.yaml'
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"a","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"},{"name":"b","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]' | python3 tools/check-manifests.py
	helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--log.level=debug"]' --set-json 'extraVolumes=[{"name":"extra-collectors","configMap":{"name":"my-collectors"}}]' --set-json 'extraVolumeMounts=[{"name":"extra-collectors","mountPath":"/etc/collectors","readOnly":true}]'
	@# Packaged into a temporary directory: a .tgz in the worktree is build
	@# output, and the release workflow is what publishes one.
	@set -e; dist=$$(mktemp -d); \
	helm package charts/prometheus-universal-exporter --destination "$$dist" >/dev/null; \
	package=$$(ls "$$dist"/prometheus-universal-exporter-*.tgz); \
	helm template test "$$package" >/dev/null; \
	helm install test "$$package" --dry-run=client >/dev/null; \
	rm -rf "$$dist"
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
	@for bad in 'replicaCount=many' 'image.pullPolicy=always' 'replicaCounts=2'; do \
		if helm template test charts/prometheus-universal-exporter --set "$$bad" >/dev/null 2>&1; then \
			echo "helm template accepted '$$bad', which values.schema.json should reject" >&2; \
			exit 1; \
		fi; \
	done
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--web.listen-address=:9999"]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraArgs entry overriding a chart-managed flag" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--dry-run"]' >/dev/null 2>&1; then \
		echo "helm template accepted --dry-run in extraArgs, which would make the pod exit instead of serving" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["log.level=debug"]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraArgs entry that is not a flag" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraVolumes=[{"name":"shadow","configMap":{"name":"shadow"}}]' --set-json 'extraVolumeMounts=[{"name":"shadow","mountPath":"/etc/prometheus-universal-exporter"}]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraVolumeMounts entry hiding the configuration directory" >&2; \
		exit 1; \
	fi

ci: fmt-check lint test vet build helm-test
