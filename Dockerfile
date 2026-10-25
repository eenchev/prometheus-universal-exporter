ARG GO_VERSION=1.27
ARG PYTHON_VERSION=3.12
ARG LXML_VERSION=6.1.3
ARG PYYAML_VERSION=6.0.3
ARG PYTHON_DATEUTIL_VERSION=2.9.0.post0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The build arguments are declared here, after the module download, not at the
# top of the stage: every RUN after an ARG depends on its value, and REVISION
# changes with every commit, so declared earlier it would make go mod download
# miss the layer cache on every build. TARGETOS and TARGETARCH come here too,
# so one download serves every platform the image is built for.
ARG TARGETOS
ARG TARGETARCH
# REQUEST_TYPES selects the request types built in, as a comma-separated list
# such as "http". Empty, the default, builds every type but the opt-in ones.
ARG REQUEST_TYPES
# OPT_IN_REQUEST_TYPES adds the opt-in request types, which the published
# image leaves out, as a comma-separated list such as "s3file" or
# "s3file,redis,tcp,ldap".
ARG OPT_IN_REQUEST_TYPES
# SQL_DRIVERS adds the sql request type's opt-in database drivers, as a
# comma-separated list such as "oracle,snowflake". Empty, the default, the
# type has its built-in mysql, postgres and sqlserver drivers alone.
ARG SQL_DRIVERS
# VERSION is the release this image is, reported by --version and the
# http_exporter_build_info self-metric. Empty, the version Go stamps is used.
ARG VERSION
# REVISION is the commit the image is built from, reported the same way. The
# build stage has no git, and the build context no .git (.dockerignore), so Go
# cannot stamp it itself; the release workflow passes the tag's commit.
ARG REVISION

RUN tags="$(sh tools/request-type-tags.sh "${REQUEST_TYPES}" "${SQL_DRIVERS}" "${OPT_IN_REQUEST_TYPES}")" || exit 1; \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" -tags "${tags}" \
    -o /out/prometheus-universal-exporter . \
    && CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" -tags "${tags}" \
    -o /out/pue-allocator ./cmd/pue-allocator

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
# tzdata gives Python scripts the named time zones zoneinfo and dateutil.tz
# read, the one kind of file the Python sandbox lets a script open.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y --no-install-recommends \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends tzdata \
    && pip install --no-cache-dir \
    lxml==${LXML_VERSION} \
    PyYAML==${PYYAML_VERSION} \
    python-dateutil==${PYTHON_DATEUTIL_VERSION} \
    && python -m pip uninstall -y pip \
    && rm -rf /usr/local/lib/python3*/ensurepip/_bundled \
    && groupadd --system --gid 65532 exporter \
    && useradd --system --uid 65532 --gid exporter exporter \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/prometheus-universal-exporter /bin/prometheus-universal-exporter
# The target allocator, run from the same image with its own entrypoint,
# /bin/pue-allocator (docs/ALLOCATOR.md).
COPY --from=build /out/pue-allocator /bin/pue-allocator

# By number, not name: Kubernetes checks runAsNonRoot against the image's
# user only when it is numeric, and refuses to start a named one.
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/bin/prometheus-universal-exporter"]

CMD ["--config.file=/etc/prometheus-universal-exporter/config.yaml", "--web.listen-address=:8080"]