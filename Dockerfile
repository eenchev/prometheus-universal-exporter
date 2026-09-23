ARG GO_VERSION=1.27
ARG PYTHON_VERSION=3.12
ARG LXML_VERSION=6.1.3
ARG PYYAML_VERSION=6.0.2
ARG PYTHON_DATEUTIL_VERSION=2.9.0.post0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH
# REQUEST_TYPES selects the request types built in, as a comma-separated list
# such as "http". Empty, the default, builds every type.
ARG REQUEST_TYPES
# VERSION is the release this image is, reported by --version and the
# http_exporter_build_info self-metric. Empty, the version Go stamps is used.
ARG VERSION

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN tags="$(sh tools/request-type-tags.sh "${REQUEST_TYPES}")" || exit 1; \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -tags "${tags}" \
    -o /out/prometheus-universal-exporter .

FROM python:${PYTHON_VERSION}-slim

LABEL org.opencontainers.image.source="https://github.com/eenchev/prometheus-universal-exporter"
LABEL org.opencontainers.image.description="Prometheus Universal Exporter"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ARG LXML_VERSION
ARG PYYAML_VERSION
ARG PYTHON_DATEUTIL_VERSION

# Debian security fixes are applied at build time rather than waiting for the
# next python image, so a rebuild picks them up as soon as Debian ships them.
# pip is removed once the libraries are in: the exporter never installs a
# package at runtime, so pip would only be attack surface, and its CVEs would
# be reported against the image.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y --no-install-recommends \
    && pip install --no-cache-dir \
    lxml==${LXML_VERSION} \
    PyYAML==${PYYAML_VERSION} \
    python-dateutil==${PYTHON_DATEUTIL_VERSION} \
    && python -m pip uninstall -y pip \
    && rm -rf /usr/local/lib/python3*/ensurepip/_bundled \
    && groupadd --system exporter \
    && useradd --system --gid exporter exporter \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/prometheus-universal-exporter /bin/prometheus-universal-exporter

USER exporter

EXPOSE 8080

ENTRYPOINT ["/bin/prometheus-universal-exporter"]

CMD ["--config.file=/etc/prometheus-universal-exporter/config.yaml", "--web.listen-address=:8080"]