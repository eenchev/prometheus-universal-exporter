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

ci: fmt test vet build helm-test
