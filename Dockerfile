ARG GO_VERSION=1.27
ARG PYTHON_VERSION=3.12
ARG BEAUTIFULSOUP4_VERSION=4.12.3
ARG LXML_VERSION=5.3.0
ARG PYYAML_VERSION=6.0.2
ARG PYTHON_DATEUTIL_VERSION=2.9.0.post0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/prometheus-universal-exporter .

FROM python:${PYTHON_VERSION}-slim

LABEL org.opencontainers.image.source="https://github.com/eenchev/prometheus-universal-exporter"
LABEL org.opencontainers.image.description="Prometheus Universal Exporter"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ARG BEAUTIFULSOUP4_VERSION
ARG LXML_VERSION
ARG PYYAML_VERSION
ARG PYTHON_DATEUTIL_VERSION

RUN pip install --no-cache-dir \
    beautifulsoup4==${BEAUTIFULSOUP4_VERSION} \
    lxml==${LXML_VERSION} \
    PyYAML==${PYYAML_VERSION} \
    python-dateutil==${PYTHON_DATEUTIL_VERSION} \
    && groupadd --system exporter \
    && useradd --system --gid exporter exporter \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/prometheus-universal-exporter /bin/prometheus-universal-exporter

USER exporter

EXPOSE 8080

ENTRYPOINT ["/bin/prometheus-universal-exporter"]

CMD ["--config.file=/etc/prometheus-universal-exporter/config.yaml", "--web.listen-address=:8080"]