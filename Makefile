APP := prometheus-universal-exporter
# The target allocator's binary, built from cmd/pue-allocator beside the
# exporter's, into bin/ and into the image. See docs/ALLOCATOR.md.
ALLOCATOR := pue-allocator

# Must be a release that supports the Go the build uses: an older one cannot
# read a newer toolchain's standard library or export data (v2.13.2 against Go
# 1.27.2). Kept in step with .github/workflows/ci.yml by a test.
GOLANGCI_LINT_VERSION := v2.14.0
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

.PHONY: build run build-request-types build-sql-drivers build-opt-in-types test-opt-in-types lint-opt-in-types lint-request-types ci test test-request-types test-scaffold test-sql-drivers test-sql-live test-external vet fmt fmt-check lint lint-version lint-install gopls-check gopls-version gopls-install helm-version helm-install hooks precommit vulncheck helm-test schemas lint-sql-drivers new-request-type
# REQUEST_TYPES builds only the listed request types, comma-separated, for
# example `make build REQUEST_TYPES=http`. Empty, the default, builds every type
# but the opt-in ones.
# See "Choosing request types at build time" in docs/CONFIGURATION.md.
REQUEST_TYPES ?=
# OPT_IN_REQUEST_TYPES adds the opt-in request types, comma-separated, which
# no build carries unless asked: `make build OPT_IN_REQUEST_TYPES=s3file,redis,tcp,ldap`.
# See docs/S3FILE.md, docs/REDIS.md, docs/TCP.md and docs/LDAP.md.
OPT_IN_REQUEST_TYPES ?=
# SQL_DRIVERS adds the sql request type's opt-in database drivers,
# comma-separated, for example `make build SQL_DRIVERS=oracle,snowflake`: any
# of oracle, clickhouse, snowflake and vertica. Empty, the default, the sql
# type has its three built-in drivers alone, mysql, postgres and sqlserver.
# See "Building with more drivers" in docs/SQL.md.
SQL_DRIVERS ?=

# The exporter's binary, bin/$(APP), built from the main package, and the
# target allocator's, bin/$(ALLOCATOR), with the same request types: the
# allocator checks the configuration it serves with the exporter's loader.
build:
	tags="$$(sh tools/request-type-tags.sh '$(REQUEST_TYPES)' '$(SQL_DRIVERS)' '$(OPT_IN_REQUEST_TYPES)')" && go build -tags "$$tags" -o bin/$(APP) . && go build -tags "$$tags" -o bin/$(ALLOCATOR) ./cmd/$(ALLOCATOR)
	@sh tools/run-example.sh check-root-binaries $(APP) $(ALLOCATOR)

# EXAMPLE names the shipped example `make run` runs: a directory under
# examples/ by its name, such as open-meteo, or examples/config.<name>.*.yaml
# by <name>, such as ecb; `make run` alone lists them. The exporter listens on
# HOST:PORT, this machine alone by default, as an example is something to try
# out, not to serve; an IPv6 HOST may be bare (::1) or bracketed. ARGS adds
# flags, such as ARGS=--log.level=debug. Set with = rather than ?=, so that
# only the command line sets them: zsh exports HOST as the machine's name,
# which ?= would take.
EXAMPLE =
HOST = 127.0.0.1
PORT = 8080
ARGS =

# Builds as `make build` does, with the opt-in request types the example
# needs added to OPT_IN_REQUEST_TYPES, so a binary built before a pull is
# never what runs, and runs bin/$(APP) with the example's configuration and
# static target file. See tools/run-example.sh.
run:
	@if [ -z '$(EXAMPLE)' ]; then \
		echo "usage: make run EXAMPLE=<name> [PORT=8080] [HOST=127.0.0.1] [ARGS=--log.level=debug]; the examples are:" >&2; \
		sh tools/run-example.sh list >&2; \
		exit 2; \
	fi
	@sh tools/run-example.sh check-host '$(HOST)' && \
		types="$$(sh tools/run-example.sh opt-in-types '$(EXAMPLE)' '$(OPT_IN_REQUEST_TYPES)')" && \
		$(MAKE) --no-print-directory build OPT_IN_REQUEST_TYPES="$$types"
	@sh tools/run-example.sh run '$(EXAMPLE)' '$(HOST)' '$(PORT)' $(ARGS)

# What ci.yml's build steps run: every package compiled, then each request
# type vetted and built on its own, so code one type needs cannot quietly
# depend on another. Nothing is kept. A test keeps it running what ci.yml
# runs and `make ci` including it.
build-request-types:
	go build ./...
	@set -e; for type in dns graphite grpc http kubernetes ldap localfile ntp redis s3file sql sshexec sshfile tcp tlscert; do \
		tags="$$(sh tools/request-type-tags.sh "$$type")"; \
		echo "go vet -tags $$tags ./... && go build -tags $$tags ./..."; \
		go vet -tags "$$tags" ./...; \
		go build -tags "$$tags" ./...; \
	done

# What ci.yml's build step of the opt-in sql drivers runs: every package
# vetted and built with all four, which no other build compiles, as the
# default build compiles the three built-in ones. A test keeps the list
# naming every opt-in driver in the tree.
build-sql-drivers:
	tags="$$(sh tools/request-type-tags.sh '' oracle,clickhouse,snowflake,vertica)" && go vet -tags "$$tags" ./... && go build -tags "$$tags" ./...

# golangci-lint over the build with every opt-in sql driver, as ci.yml's second
# lint step runs it: no other lint run compiles them, and a build with more
# packages can give the linters' analyses more to see.
lint-sql-drivers: lint-version
	tags="$$(sh tools/request-type-tags.sh '' oracle,clickhouse,snowflake,vertica)" && golangci-lint run --build-tags "$$tags"

# The tests of the build with every opt-in sql driver, as ci.yml runs them:
# the drivers' own tests of how a connection is configured are in files
# built only with them. Once and without -race.
test-sql-drivers:
	tags="$$(sh tools/request-type-tags.sh '' oracle,clickhouse,snowflake,vertica)" && go test -count=1 -tags "$$tags" ./internal/fetch/

# What ci.yml's build step of the opt-in request types runs: every package
# vetted and built with every type of the default build and every opt-in
# type besides, which no other build compiles together, as a build with
# OPT_IN_REQUEST_TYPES is. A test keeps the list naming every opt-in type in
# the tree.
build-opt-in-types:
	tags="$$(sh tools/request-type-tags.sh '' '' s3file,redis,tcp,ldap,sshexec)" && go vet -tags "$$tags" ./... && go build -tags "$$tags" ./...

# golangci-lint over the build with every opt-in request type, as ci.yml's
# third lint step runs it: no other lint run compiles them beside the
# default build's types.
lint-opt-in-types: lint-version
	tags="$$(sh tools/request-type-tags.sh '' '' s3file,redis,tcp,ldap,sshexec)" && golangci-lint run --build-tags "$$tags"

# golangci-lint over each request type's build on its own, as ci.yml's
# request-types matrix runs it, a job for each type: a helper a one-type
# build leaves unused, such as one in an untagged file only another type's
# tests call, is reported by no lint of a build with every type. A test
# keeps the loop naming every type in the tree.
lint-request-types: lint-version
	@set -e; for type in dns graphite grpc http kubernetes ldap localfile ntp redis s3file sql sshexec sshfile tcp tlscert; do \
		tags="$$(sh tools/request-type-tags.sh "$$type")"; \
		echo "golangci-lint run --build-tags $$tags"; \
		golangci-lint run --build-tags "$$tags"; \
	done

# The internal/fetch and repository tests of the build with every opt-in
# request type, as ci.yml runs them: the opt-in types' tests beside the
# default build's, and their documentation's examples and schema rows,
# which a build of one type's tests reach only through make. Once and
# without -race.
test-opt-in-types:
	tags="$$(sh tools/request-type-tags.sh '' '' s3file,redis,tcp,ldap,sshexec)" && go test -count=1 -tags "$$tags" ./test/repository/ ./internal/fetch/

# Opt-in: the sql request type against real databases, which the
# PUE_TEST_POSTGRES, PUE_TEST_MYSQL and PUE_TEST_SQLSERVER variables name
# (host:port), each with its _USER and _PASSWORD; a database without its
# variable is skipped. ci.yml's sql-live job, which ran it against service
# containers, is commented out for now. See docs/DEVELOPMENT.md.
test-sql-live:
	go test -count=1 -run TestLiveSQL -v ./internal/fetch/

# The scaffold test builds a copy of the tree, minutes of work, so `test`
# leaves it to `test-scaffold`, as ci.yml leaves it to a job of its own.
test:
	go test -skip '^TestANewRequestTypeIsWiredByTheScaffold$$' ./...
	@# Twice, in a random order, so a test that depends on the order tests run
	@# in, or on running only once, is caught. The limit is a package's: go
	@# test's own ten minutes are for one run without the race detector, and
	@# this is two with it, on whatever machine CI was given. A test keeps
	@# ci.yml running the same command.
	go test -race -count=2 -shuffle=on -timeout 20m ./...

# make new-request-type on a copy of the tree, which the copy is then built
# and tested with (test/repository/newrequesttype_test.go), as ci.yml's job
# scaffold runs it.
test-scaffold:
	go test -run '^TestANewRequestTypeIsWiredByTheScaffold$$' ./test/repository/

# The tests of each request type built on its own, as ci.yml runs them: a
# build with REQUEST_TYPES=http is one the exporter ships, so its tests have
# to pass as the default build's do. Once and without -race, which `test`
# covers with every type. A test keeps the loop naming every type in the tree
# and running what ci.yml runs.
test-request-types:
	@set -e; for type in dns graphite grpc http kubernetes ldap localfile ntp redis s3file sql sshexec sshfile tcp tlscert; do \
		tags="$$(sh tools/request-type-tags.sh "$$type")"; \
		echo "go test -count=1 -tags $$tags ./..."; \
		go test -count=1 -tags "$$tags" ./...; \
	done

# Opt-in: probes real third-party endpoints, so it is deliberately not part of
# `make ci`. See docs/DEVELOPMENT.md.
test-external:
	EXTERNAL_E2E=1 go test -run TestExternal -v ./...

# Adds a request type to the tree, NAME its name and OPT_IN=1 for an opt-in
# one: its files, with a Requester that answers "not implemented" until its
# logic is written, and the wiring that names every type, then make schemas
# (tools/new-request-type.sh; docs/DEVELOPMENT.md, Adding a request type). It
# changes the tree, so make ci does not run it.
new-request-type:
	@test -n '$(NAME)' || { echo "usage: make new-request-type NAME=<name> [OPT_IN=1] [GO_NAME=<GoName>]" >&2; exit 1; }
	sh tools/new-request-type.sh '$(NAME)' $(if $(OPT_IN),opt-in,default) '$(GO_NAME)'

# The committed JSON Schemas are generated from the configuration structs; a
# test fails when one is out of date. Run this after changing a key. The
# catalogue of the request types' own keys, which every build reads the keys
# of the types it leaves out from, is made first, from a build with every
# request type.
schemas:
	tags="$$(sh tools/request-type-tags.sh '' '' s3file,redis,tcp,ldap,sshexec)" && go test -count=1 -tags "$$tags" -run '^TestTheRequestKeyCatalogueIsCurrent$$' ./internal/config/ -args -update-request-keys
	go run . --config.schema > configs/config.schema.json
	go run . --config.collector-file-schema > configs/collector-file.schema.json
	go run . --static-targets-file-schema > configs/static-targets.schema.json
	go run ./cmd/pue-allocator --static-targets-file-schema > configs/allocator-static-targets.schema.json
	go run ./cmd/pue-allocator --probe-targets-file-schema > configs/probe-targets.schema.json

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
# and not part of `make ci`. What is scanned is what ships and what the
# workflows run: the exporter and the repository's tools, each with all it
# imports. The packages only tests use, such as the stand-in gRPC server of
# internal/grpctest, are left out: a vulnerability only a test can reach is
# none of the exporter's. A test keeps the packages equal to the workflow's
# and the list of what is left out complete.
VULNCHECK_PACKAGES := . ./cmd/... ./tools/...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(VULNCHECK_PACKAGES)

# The installed helm has to be the pinned release. A helm built from source
# without the version stamped in (helm-install) reports only its release line,
# "v4.3", which is read as its .0 release, v4.3.0: that is what a source build
# of a .0 pin reports, though the line alone cannot tell which patch of it was
# built. A version with a patch is compared whole, so v4.3.1 is no v4.3.0.
helm-version:
	@command -v helm >/dev/null 2>&1 || { \
		echo "helm is not installed; run: make helm-install" >&2; exit 1; }
	@have=$$(helm version --short | sed 's/+.*//'); \
	full=$$have; \
	case "$$have" in v*.*.*) ;; v*.*) full=$$have.0 ;; esac; \
	if [ "$$full" != '$(HELM_VERSION)' ]; then \
		echo "helm is $$have but CI runs $(HELM_VERSION); run: make helm-install" >&2; \
		exit 1; \
	fi

# `go install` builds helm from its source, where the version helm reports is
# the release line, "v4.3", until a release build stamps the full one into it;
# helm's own Makefile does that with this -X. helm-version takes a release
# line alone as its .0 release, so such a helm of a .0 pin passes without the
# stamp, but one of any other pin would not, and with it helm reports the
# version it is.
helm-install:
	go install -ldflags "-X helm.sh/helm/v4/internal/version.version=$(HELM_VERSION)" helm.sh/helm/v4/cmd/helm@$(HELM_VERSION)

# The check ci.yml makes of the monitor rendered with webAuth, word for word.
# The probe monitor itself, not only the self-metrics one, has to present the
# credential, or every probe it sends is a 401.
define HELM_TEST_WEBAUTH_MONITOR
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
probe = [d for d in docs if d.get("kind") == "ServiceMonitor" and d["metadata"]["name"] == "test-prometheus-universal-exporter-t"]
if len(probe) != 1:
    sys.exit(f"{len(probe)} probe monitors named test-prometheus-universal-exporter-t were rendered")
auth = probe[0]["spec"]["endpoints"][0].get("basicAuth") or {}
if auth.get("username", {}).get("name") != "exporter-auth" or auth.get("password", {}).get("name") != "exporter-auth":
    sys.exit(f"the probe monitor does not present the webAuth Secret: {auth}")
endef
export HELM_TEST_WEBAUTH_MONITOR

# Every chart step of .github/workflows/ci.yml, in its order: the same
# renders, the same lines looked for in them and the same rejections, so a
# local run means a CI run. A test fails when ci.yml renders, rejects or looks
# for something this target does not.
helm-test: helm-version
	helm lint charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"service-targets","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"pod-targets","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]'
	helm template test charts/prometheus-universal-exporter --set server.listenAddress=0.0.0.0:9115 --set server.pythonPath=/usr/bin/python3.11
	helm template test charts/prometheus-universal-exporter --set server.expandEnv=true --set-json 'env=[{"name":"DEMO_TARGET","value":"http://api.internal:8080"}]' --set-json 'envFrom=[{"secretRef":{"name":"exporter-secrets"}}]'
	helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=configs/static-targets.example.yaml --set-file 'config.data.config\.yaml=configs/config.otlp.example.yaml'
	helm template test charts/prometheus-universal-exporter --set server.logLevel=debug --set server.probeTimeoutOffset=1s --set server.probeDefaultTimeout=45s
	@# The shutdown delay and timeout, their grace period and the exporter
	@# credential Secret.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set server.shutdownTimeout=1m)"; \
	echo "$$out" | grep -q -- '--web.shutdown-timeout=1m'; \
	echo "$$out" | grep -q 'terminationGracePeriodSeconds: 75'; \
	out="$$(helm template test charts/prometheus-universal-exporter --set server.shutdownTimeout=15s)"; \
	if echo "$$out" | grep -q terminationGracePeriodSeconds; then \
		echo "a grace period was rendered where Kubernetes' default covers the shutdown" >&2; \
		exit 1; \
	fi; \
	out="$$(helm template test charts/prometheus-universal-exporter)"; \
	echo "$$out" | grep -q -- '--web.shutdown-delay=5s'; \
	if echo "$$out" | grep -q terminationGracePeriodSeconds; then \
		echo "the default shutdown delay and timeout rendered a grace period" >&2; \
		exit 1; \
	fi; \
	out="$$(helm template test charts/prometheus-universal-exporter --set server.shutdownDelay=20s)"; \
	echo "$$out" | grep -q -- '--web.shutdown-delay=20s'; \
	echo "$$out" | grep -q 'terminationGracePeriodSeconds: 45'; \
	helm template test charts/prometheus-universal-exporter --set server.shutdownDelay=0s | grep -q -- '--web.shutdown-delay=0s'; \
	out="$$(helm template test charts/prometheus-universal-exporter --set server.shutdownDelay=)"; \
	if echo "$$out" | grep -q -- '--web.shutdown-delay'; then \
		echo "an empty server.shutdownDelay rendered the flag" >&2; \
		exit 1; \
	fi; \
	if helm template test charts/prometheus-universal-exporter --set server.shutdownDelay=10s --set terminationGracePeriodSeconds=20 >/dev/null 2>&1; then \
		echo "helm template accepted a grace period shorter than the shutdown delay and timeout" >&2; \
		exit 1; \
	fi; \
	out="$$(helm template test charts/prometheus-universal-exporter --set webAuth.enabled=true --set webAuth.secretName=exporter-auth --set-json 'monitors=[{"name":"t","enabled":true,"type":"service","collector":"example"}]')"; \
	echo "$$out" | grep -q 'mountPath: "/var/run/prometheus-universal-exporter/web-auth"'; \
	echo "$$out" | grep -q 'secretName: "exporter-auth"'; \
	echo "$$out" | python3 -c "$$HELM_TEST_WEBAUTH_MONITOR"
	helm template test charts/prometheus-universal-exporter --set-json 'extraArgs=["--some.new-flag=value"]' --set-json 'extraVolumes=[{"name":"extra-collectors","configMap":{"name":"my-collectors"}}]' --set-json 'extraVolumeMounts=[{"name":"extra-collectors","mountPath":"/etc/collectors","readOnly":true}]'
	@# Every rendered manifest is its own document.
	helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"a","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s"},{"name":"b","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s"}]' | python3 tools/check-manifests.py
	@# Static targets without OTLP, and the monitor of their endpoint.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=testdata/chart/static-targets-endpoint-only.yaml)"; \
	echo "$$out" | grep -q -- '--static-targets-file=/etc/prometheus-universal-exporter-static-targets/static-targets.yaml'; \
	echo "$$out" | grep -q -- '--web.static-targets-path=/static-targets'; \
	echo "$$out" | grep -q 'name: .*-static-targets$$'; \
	echo "$$out" | grep -q 'path: "/static-targets"'; \
	out="$$(helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=testdata/chart/static-targets-endpoint-only.yaml --set staticTargets.path=/targets --set staticTargets.monitor.type=pod)"; \
	echo "$$out" | grep -q -- '--web.static-targets-path=/targets'; \
	echo "$$out" | grep -q 'kind: PodMonitor'; \
	out="$$(helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=testdata/chart/static-targets-endpoint-only.yaml --set staticTargets.monitor.enabled=false)"; \
	if echo "$$out" | grep -q 'kind: .*Monitor$$'; then \
		echo "the static targets monitor rendered with staticTargets.monitor.enabled false" >&2; \
		exit 1; \
	fi; \
	out="$$(helm template test charts/prometheus-universal-exporter)"; \
	if echo "$$out" | grep -q -- '--static-targets-file'; then \
		echo "--static-targets-file rendered without staticTargets.enabled" >&2; \
		exit 1; \
	fi
	@# The Recreate strategy renders no rollingUpdate, which Kubernetes refuses
	@# beside it.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate)"; \
	echo "$$out" | grep -q 'type: Recreate'; \
	if echo "$$out" | grep -q rollingUpdate; then \
		echo "strategy.type Recreate rendered a rollingUpdate" >&2; \
		exit 1; \
	fi
	@# The garbage collector's target: goGC.percent renders GOGC, and the
	@# default values render none.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)"; \
	echo "$$out" | grep -q 'name: GOGC'; \
	echo "$$out" | grep -q 'value: "400"'; \
	out="$$(helm template test charts/prometheus-universal-exporter)"; \
	if echo "$$out" | grep -q GOGC; then \
		echo "the default values rendered GOGC" >&2; \
		exit 1; \
	fi
	@# A whole number is rendered as the whole number it is, however it is
	@# written: --set-json hands the templates a floating-point number, as a
	@# values file does, which a template prints as 2e+06 by itself. One past
	@# what a count may be is rejected.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set-json replicaCount=2000000 --set-json server.probeMaxConcurrent=2e6 --set-json server.pythonMaxWorkers=2000000.0)"; \
	echo "$$out" | grep -q 'replicas: 2000000$$'; \
	echo "$$out" | grep -q -- '--probe.max-concurrent=2000000"'; \
	echo "$$out" | grep -q -- '--python.max-workers=2000000"'; \
	if echo "$$out" | grep -q 'e+0'; then \
		echo "a whole number was rendered with an exponent" >&2; \
		exit 1; \
	fi; \
	if helm template test charts/prometheus-universal-exporter --set-json replicaCount=2147483648 >/dev/null 2>&1; then \
		echo "helm template accepted a replicaCount past 2147483647" >&2; \
		exit 1; \
	fi
	@# A monitor's port and namespaces.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"t","enabled":true,"type":"service","collector":"example","interval":"30s","scrapeTimeout":"10s","port":"grpc","namespaceSelector":{"matchNames":["a"]}}]')"; \
	echo "$$out" | grep -q 'port: "grpc"'; \
	echo "$$out" | grep -q 'namespaceSelector:'; \
	echo "$$out" | grep -q '^    - a$$'; \
	out="$$(helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"t","enabled":true,"type":"pod","collector":"example","interval":"30s","scrapeTimeout":"10s","port":"grpc","namespaceSelector":{"matchNames":["a"]}}]')"; \
	echo "$$out" | grep -q 'kind: PodMonitor'; \
	echo "$$out" | grep -q 'port: "grpc"'; \
	echo "$$out" | grep -q 'namespaceSelector:'; \
	echo "$$out" | grep -q '^    - a$$'
	@# The target allocator: its Deployment and the exporter in allocator
	@# mode, with the ScrapeConfig of a probe job; and what the allocator
	@# cannot run with is rejected.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set allocator.enabled=true --set-json 'allocator.probeJobs=[{"name":"websites","collector":"example","targets":["https://shop.example"]}]')"; \
	echo "$$out" | grep -q 'command: \["/bin/pue-allocator"\]'; \
	echo "$$out" | grep -q -- '--allocator.url=http://test-prometheus-universal-exporter-allocator.default.svc:9116'; \
	echo "$$out" | grep -q 'url: "http://test-prometheus-universal-exporter-allocator.default.svc:9116/v1/sd/websites"'; \
	if echo "$$out" | grep -q -- '- --config.file='; then \
		echo "the exporter in allocator mode was given --config.file" >&2; \
		exit 1; \
	fi; \
	echo "$$out" | python3 tools/check-manifests.py; \
	if helm template test charts/prometheus-universal-exporter --set allocator.enabled=true --set staticTargets.enabled=true --set-file staticTargets.data=testdata/chart/static-targets-endpoint-only.yaml >/dev/null 2>&1; then \
		echo "helm template accepted the allocator beside staticTargets.enabled" >&2; \
		exit 1; \
	fi; \
	if helm template test charts/prometheus-universal-exporter --set allocator.enabled=true --set-json 'monitors=[{"name":"t","enabled":true,"type":"service","collector":"example"}]' >/dev/null 2>&1; then \
		echo "helm template accepted a probe monitor beside the allocator" >&2; \
		exit 1; \
	fi
	@# Autoscaling on the exporter's own load and, with the allocator, on
	@# the static targets per pod, as Pods metrics; the second without the
	@# allocator is rejected.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set allocator.enabled=true --set autoscaling.enabled=true --set autoscaling.targetBusy=2 --set autoscaling.targetStaticTargetsPerPod=40)"; \
	echo "$$out" | grep -q 'name: "http_exporter_busy"'; \
	echo "$$out" | grep -q 'name: "http_exporter_static_targets"'; \
	echo "$$out" | grep -q 'averageValue: "40"'; \
	echo "$$out" | python3 tools/check-manifests.py; \
	if helm template test charts/prometheus-universal-exporter --set autoscaling.enabled=true --set autoscaling.targetStaticTargetsPerPod=40 >/dev/null 2>&1; then \
		echo "helm template accepted autoscaling.targetStaticTargetsPerPod without the allocator" >&2; \
		exit 1; \
	fi
	@# A configuration file whose first line is indented. The render is kept
	@# first: a pipeline would hide helm failing.
	@set -eu; \
	out="$$(helm template test charts/prometheus-universal-exporter --set-file 'config.data.extra\.yaml=testdata/chart/indented-first-line.yaml')"; \
	echo "$$out" | grep -q '^      # A file whose first line is indented'; \
	echo "$$out" | python3 tools/check-manifests.py
	@# The chart as a dependency of a parent chart. helm dependency build
	@# writes into the parent, so it is built in a copy of the two charts, laid
	@# out as they are here for its file:// path to reach the chart.
	@set -eu; tree=$$(mktemp -d); trap 'rm -rf "$$tree"' EXIT; \
	mkdir -p "$$tree/charts" "$$tree/testdata/chart"; \
	cp -R charts/prometheus-universal-exporter "$$tree/charts/"; \
	cp -R testdata/chart/parent "$$tree/testdata/chart/"; \
	helm dependency build "$$tree/testdata/chart/parent" >/dev/null; \
	out="$$(helm template test "$$tree/testdata/chart/parent" --set prometheus-universal-exporter.enabled=true)"; \
	echo "$$out" | grep -q '^# Source: parent/charts/prometheus-universal-exporter/templates/deployment.yaml$$'; \
	echo "$$out" | grep -q 'replicas: 2'
	@# Static targets exported over OTLP are rejected without OTLP export.
	@if helm template test charts/prometheus-universal-exporter --set staticTargets.enabled=true --set-file staticTargets.data=configs/static-targets.example.yaml --set-file 'config.data.config\.yaml=configs/config.example.yaml' >/dev/null 2>&1; then \
		echo "helm template accepted a static target with export_via_otlp while OTLP export is disabled" >&2; \
		exit 1; \
	fi
	@# extraArgs and mount paths that collide with the chart are rejected, and
	@# so is every value the chart validates itself.
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
	@for bad in server.logLevel=verbose server.probeTimeoutOffset=-1s server.probeDefaultTimeout=-1s selfMetrics.path=/probe selfMetrics.path=/stats/ staticTargets.path=/self-metrics staticTargets.path=/probe staticTargets.monitor.type=node server.shutdownTimeout=500ms server.shutdownTimeout=0s server.shutdownDelay=500ms server.shutdownDelay=-1s terminationGracePeriodSeconds=10 webAuth.enabled=true selfMetrics.path=/static-targets/status; do \
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
	@if helm template test charts/prometheus-universal-exporter --set-json 'extraVolumes=[{"name":"shadow","configMap":{"name":"shadow"}}]' --set-json 'extraVolumeMounts=[{"name":"shadow","mountPath":"/etc/prometheus-universal-exporter/"}]' >/dev/null 2>&1; then \
		echo "helm template accepted an extraVolumeMounts entry hiding the configuration directory behind a trailing slash" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set webAuth.enabled=true --set webAuth.secretName=s --set webAuth.mountPath=/etc/prometheus-universal-exporter/ >/dev/null 2>&1; then \
		echo "helm template accepted a webAuth.mountPath at the configuration directory with a trailing slash" >&2; \
		exit 1; \
	fi
	@# Monitor auth without a type, a monitor's port given as a number, a
	@# scrape timeout longer than its interval and an Ingress without the
	@# Service are rejected.
	@if helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"t","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"secretName":"s"}}]' >/dev/null 2>&1; then \
		echo "helm template accepted a monitor that enables auth without a type" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set-json 'monitors=[{"name":"t","enabled":true,"type":"service","collector":"example","port":"9115"}]' >/dev/null 2>&1; then \
		echo "helm template accepted a monitor's port given as a number, which names no port" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set selfMetrics.enabled=true --set selfMetrics.interval=10s --set selfMetrics.scrapeTimeout=5m >/dev/null 2>&1; then \
		echo "helm template accepted a selfMetrics.scrapeTimeout longer than selfMetrics.interval" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set ingress.enabled=true --set service.enabled=false >/dev/null 2>&1; then \
		echo "helm template accepted an Ingress without the Service it routes to" >&2; \
		exit 1; \
	fi
	@# An exporter reachable from outside the cluster, through an Ingress or a
	@# LoadBalancer or NodePort Service, needs the exporter to authenticate its
	@# callers: rendering fails without webAuth, and with webAuth beside a
	@# configuration whose web.basic_auth does not read its files; webAuth with
	@# one that does, or exposure.allowWithoutWebAuth, renders.
	@if helm template test charts/prometheus-universal-exporter --set ingress.enabled=true >/dev/null 2>&1; then \
		echo "helm template accepted an Ingress without webAuth" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set ingress.enabled=true --set webAuth.enabled=true --set webAuth.secretName=s >/dev/null 2>&1; then \
		echo "helm template accepted an Ingress with webAuth beside a configuration whose web.basic_auth does not read its files" >&2; \
		exit 1; \
	fi
	helm template test charts/prometheus-universal-exporter --set ingress.enabled=true --set exposure.allowWithoutWebAuth=true >/dev/null
	helm template test charts/prometheus-universal-exporter --set ingress.enabled=true --set webAuth.enabled=true --set webAuth.secretName=s --set-file 'config.data.config\.yaml=testdata/chart/web-auth.yaml' >/dev/null
	@if helm template test charts/prometheus-universal-exporter --set service.type=LoadBalancer >/dev/null 2>&1; then \
		echo "helm template accepted a LoadBalancer Service without webAuth" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set service.type=LoadBalancer --set webAuth.enabled=true --set webAuth.secretName=s >/dev/null 2>&1; then \
		echo "helm template accepted a LoadBalancer Service with webAuth beside a configuration whose web.basic_auth does not read its files" >&2; \
		exit 1; \
	fi
	helm template test charts/prometheus-universal-exporter --set service.type=LoadBalancer --set exposure.allowWithoutWebAuth=true >/dev/null
	helm template test charts/prometheus-universal-exporter --set service.type=LoadBalancer --set webAuth.enabled=true --set webAuth.secretName=s --set-file 'config.data.config\.yaml=testdata/chart/web-auth.yaml' >/dev/null
	@if helm template test charts/prometheus-universal-exporter --set service.type=NodePort >/dev/null 2>&1; then \
		echo "helm template accepted a NodePort Service without webAuth" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set service.type=NodePort --set webAuth.enabled=true --set webAuth.secretName=s >/dev/null 2>&1; then \
		echo "helm template accepted a NodePort Service with webAuth beside a configuration whose web.basic_auth does not read its files" >&2; \
		exit 1; \
	fi
	helm template test charts/prometheus-universal-exporter --set service.type=NodePort --set exposure.allowWithoutWebAuth=true >/dev/null
	helm template test charts/prometheus-universal-exporter --set service.type=NodePort --set webAuth.enabled=true --set webAuth.secretName=s --set-file 'config.data.config\.yaml=testdata/chart/web-auth.yaml' >/dev/null
	@# The static targets' status page is the static targets path followed by
	@# /status, where the self-metrics cannot be served.
	@if helm template test charts/prometheus-universal-exporter --set staticTargets.path=/static --set selfMetrics.path=/static/status >/dev/null 2>&1; then \
		echo "helm template accepted selfMetrics.path at the static targets' status page" >&2; \
		exit 1; \
	fi
	@# A garbage collector target of off with no Go memory limit in force, one
	@# of 0, and one beside a GOGC entry in env are rejected.
	@if helm template test charts/prometheus-universal-exporter --set-string goGC.percent=off --set goMemLimit.enabled=false >/dev/null 2>&1; then \
		echo "helm template accepted goGC.percent off with no Go memory limit in force" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then \
		echo "helm template accepted a goGC.percent of 0" >&2; \
		exit 1; \
	fi
	@if helm template test charts/prometheus-universal-exporter --set goGC.percent=200 --set-json 'env=[{"name":"GOGC","value":"50"}]' >/dev/null 2>&1; then \
		echo "helm template accepted goGC.percent beside a GOGC entry in env" >&2; \
		exit 1; \
	fi
	@# Values the schema does not allow are rejected.
	@for bad in 'replicaCount=many' 'image.pullPolicy=always' 'replicaCounts=2'; do \
		if helm template test charts/prometheus-universal-exporter --set "$$bad" >/dev/null 2>&1; then \
			echo "helm template accepted '$$bad', which values.schema.json should reject" >&2; \
			exit 1; \
		fi; \
	done
	@# Packaged into a temporary directory: a .tgz in the worktree is build
	@# output, and the release workflow is what publishes one.
	@set -e; dist=$$(mktemp -d); \
	helm package charts/prometheus-universal-exporter --destination "$$dist" >/dev/null; \
	package=$$(ls "$$dist"/prometheus-universal-exporter-*.tgz); \
	helm template test "$$package" >/dev/null; \
	helm install test "$$package" --dry-run=client >/dev/null; \
	rm -rf "$$dist"
	@# A server.listenAddress without a port, or on a loopback host, which
	@# neither the probes nor the Service reach.
	@for address in ':http' '9115' '0.0.0.0' ':0' '127.0.0.1:8080' 'localhost:8080' '[::1]:8080'; do \
		if helm template test charts/prometheus-universal-exporter --set "server.listenAddress=$$address" >/dev/null 2>&1; then \
			echo "helm template accepted the invalid server.listenAddress '$$address'" >&2; \
			exit 1; \
		fi; \
	done

ci: fmt-check lint lint-sql-drivers lint-opt-in-types lint-request-types gopls-check test test-scaffold vet build-request-types test-request-types build-sql-drivers test-sql-drivers build-opt-in-types test-opt-in-types helm-test
