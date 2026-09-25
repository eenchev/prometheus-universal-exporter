APP := prometheus-universal-exporter

# Must be a release built with at least the Go the build uses; an older one
# panics on standard-library sources from a newer toolchain. Kept in step with
# .github/workflows/ci.yml by a test.
GOLANGCI_LINT_VERSION := v2.13.2
# gopls is the Go language server: what VS Code and other editors show as
# problems comes from it. Some of its analyzers exist nowhere else (writestring,
# for one), so golangci-lint cannot stand in for it, and `make gopls-check`, the
# pre-commit hook and CI run `gopls check` at this version. Needs a Go at least
# as new as the release requires to install.
GOPLS_VERSION := v0.23.0
# helm renders and lints the chart; releases word errors and render details
# differently, so `make helm-test` and CI use this one. Kept in step with every
# workflow that installs helm by a test.
HELM_VERSION := v4.3.0
# Kept in step with .github/workflows/govulncheck.yml by a test.
GOVULNCHECK_VERSION := v1.8.0

.PHONY: build test test-external vet fmt fmt-check lint lint-version lint-install gopls-check gopls-version gopls-install helm-version helm-install hooks precommit vulncheck helm-test schemas
# REQUEST_TYPES builds only the listed request types, comma-separated, for
# example `make build REQUEST_TYPES=http`. Empty, the default, builds every type.
# See "Choosing request types at build time" in docs/CONFIGURATION.md.
REQUEST_TYPES ?=

build:
	tags="$$(sh tools/request-type-tags.sh '$(REQUEST_TYPES)')" && go build -tags "$$tags" ./...

test:
	go test ./...
	@# Twice, in a random order, so a test that depends on the order tests run
	@# in, or on running only once, is caught.
	go test -race -count=2 -shuffle=on ./...

# Opt-in: probes real third-party endpoints, so it is deliberately not part of
# `make ci`. See docs/DEVELOPMENT.md.
test-external:
	EXTERNAL_E2E=1 go test -run TestExternal -v ./...

# The committed JSON Schemas are generated from the configuration structs; a
# test fails when one is out of date. Run this after changing a key.
schemas:
	go run . --config.schema > configs/config.schema.json
	go run . --config.collector-file-schema > configs/collector-file.schema.json
	go run . --static-targets-file-schema > configs/static-targets.schema.json

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

# A different golangci-lint release enables different checks: an older one
# passes locally what CI then fails (gosec G705 arrived after v2.5), so lint
# refuses to run with anything but the pinned version.
lint: lint-version
	golangci-lint run

lint-version:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed; run: make lint-install" >&2; exit 1; }
	@have=$$(golangci-lint version --short); \
	want='$(GOLANGCI_LINT_VERSION)'; \
	if [ "v$${have#v}" != "$$want" ]; then \
		echo "golangci-lint is $$have but CI runs $$want; run: make lint-install" >&2; \
		exit 1; \
	fi

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Every Go file, as an editor would check it; gopls check exits 0 even when it
# reports, so any output fails the target.
gopls-check: gopls-version
	@out=$$(gopls check $$(git ls-files '*.go') 2>&1); \
	if [ -n "$$out" ]; then \
		echo "$$out"; \
		echo "gopls reports the findings above, as an editor shows them" >&2; \
		exit 1; \
	fi

gopls-version:
	@command -v gopls >/dev/null 2>&1 || { \
		echo "gopls is not installed; run: make gopls-install" >&2; exit 1; }
	@have=$$(gopls version | head -n 1 | awk '{print $$2}'); \
	have=$${have%%+*}; \
	if [ "$$have" != '$(GOPLS_VERSION)' ]; then \
		echo "gopls is $$have but CI runs $(GOPLS_VERSION); run: make gopls-install" >&2; \
		exit 1; \
	fi

gopls-install:
	go install golang.org/x/tools/gopls@$(GOPLS_VERSION)

# Points git at .githooks, whose pre-commit hook runs `make precommit`, so a
# commit that CI's lint or vet would fail is refused before it is made.
hooks:
	git config core.hooksPath .githooks

# What the pre-commit hook runs: the fast checks CI fails on, with the pinned
# linter and gopls. `make ci` remains the full run.
precommit: fmt-check lint gopls-check vet

# Known vulnerabilities the code reaches. For reference, like the CI workflow,
# and not part of `make ci`.
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

helm-version:
	@command -v helm >/dev/null 2>&1 || { \
		echo "helm is not installed; run: make helm-install" >&2; exit 1; }
	@have=$$(helm version --short | sed 's/+.*//'); \
	if [ "$$have" != '$(HELM_VERSION)' ]; then \
		echo "helm is $$have but CI runs $(HELM_VERSION); run: make helm-install" >&2; \
		exit 1; \
	fi

helm-install:
	go install helm.sh/helm/v4/cmd/helm@$(HELM_VERSION)

helm-test: helm-version
	helm lint charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"service-targets","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"pod-targets","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set server.listenAddress=0.0.0.0:9115 --set server.pythonPath=/usr/bin/python3.11
	helm template test charts/prometheus-universal-exporter --set server.expandEnv=true --set-json 'env=[{"name":"DEMO_TARGET","value":"http://api.internal:8080"}]' --set-json 'envFrom=[{"secretRef":{"name":"exporter-secrets"}}]'
	helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=configs/static-targets.example.yaml --set-file 'config.data.config\.yaml=configs/config.otlp.example.yaml'
	helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=testdata/chart/static-targets-endpoint-only.yaml
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"a","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"},{"name":"b","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]' | python3 tools/check-manifests.py
	helm template test charts/prometheus-universal-exporter --set server.logLevel=debug --set server.probeTimeoutOffset=1s --set server.probeDefaultTimeout=45s
	helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--some.new-flag=value"]' --set-json 'extraVolumes=[{"name":"extra-collectors","configMap":{"name":"my-collectors"}}]' --set-json 'extraVolumeMounts=[{"name":"extra-collectors","mountPath":"/etc/collectors","readOnly":true}]'
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
	@if helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=configs/static-targets.example.yaml --set-file 'config.data.config\.yaml=configs/config.example.yaml' >/dev/null 2>&1; then \
		echo "helm template accepted a static target with export_via_otlp while OTLP export is disabled" >&2; \
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
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--log.level=debug"]' >/dev/null 2>&1; then \
		echo "helm template accepted --log.level in extraArgs, which server.logLevel manages" >&2; \
		exit 1; \
	fi
	@for oneshot in --dry-run --config.schema --config.collector-file-schema --static-targets-file-schema --version --help; do \
		if helm template test charts/prometheus-universal-exporter --set-json "extraArgs=[\"$$oneshot\"]" >/dev/null 2>&1; then \
			echo "helm template accepted $$oneshot in extraArgs, which would make the pod exit instead of serving" >&2; \
			exit 1; \
		fi; \
	done
	@for bad in server.logLevel=verbose server.probeTimeoutOffset=-1s server.probeDefaultTimeout=-1s selfMetrics.path=/probe selfMetrics.path=/stats/ staticTargets.path=/self-metrics staticTargets.path=/probe staticTargets.monitor.type=node; do \
		if helm template test charts/prometheus-universal-exporter --set "$$bad" >/dev/null 2>&1; then \
			echo "helm template accepted $$bad" >&2; \
			exit 1; \
		fi; \
	done
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["log.level=debug"]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraArgs entry that is not a flag" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraVolumes=[{"name":"shadow","configMap":{"name":"shadow"}}]' --set-json 'extraVolumeMounts=[{"name":"shadow","mountPath":"/etc/prometheus-universal-exporter"}]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraVolumeMounts entry hiding the configuration directory" >&2; \
		exit 1; \
	fi

ci: fmt-check lint gopls-check test vet build helm-test
