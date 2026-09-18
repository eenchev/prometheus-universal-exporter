APP := prometheus-universal-exporter

GOLANGCI_LINT_VERSION := v2.5.0

.PHONY: build test vet fmt fmt-check lint lint-install helm-test
build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w *.go

fmt-check:
	@unformatted=$$(gofmt -l *.go); \
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
	helm template test charts/prometheus-universal-exporter --set serviceMonitor.enabled=true
	helm template test charts/prometheus-universal-exporter --set podMonitor.enabled=true
	helm template test charts/prometheus-universal-exporter --set server.listenAddress=0.0.0.0:9115 --set server.pythonPath=/usr/bin/python3.11
	helm template test charts/prometheus-universal-exporter --set otlpTargets.enabled=true --set-file otlpTargets.data=targets.example.yaml --set-file 'config.data.config\.yaml=config.otlp.example.yaml'

ci: fmt-check lint test vet build helm-test
