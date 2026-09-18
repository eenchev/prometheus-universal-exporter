APP := prometheus-universal-exporter

.PHONY: build test vet fmt helm-test
build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w *.go

helm-test:
	helm lint charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter
	helm template test charts/prometheus-universal-exporter --set serviceMonitor.enabled=true
	helm template test charts/prometheus-universal-exporter --set podMonitor.enabled=true
	helm template test charts/prometheus-universal-exporter --set server.listenAddress=0.0.0.0:9115 --set server.pythonPath=/usr/bin/python3.11
	helm template test charts/prometheus-universal-exporter --set otlpTargets.enabled=true --set-file otlpTargets.data=targets.example.yaml --set-file config.data.config\.yaml=config.otlp.example.yaml

ci: fmt test vet build helm-test
