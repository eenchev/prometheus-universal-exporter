FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/prometheus-universal-exporter .

FROM python:3.12-slim
RUN pip install --no-cache-dir \
    beautifulsoup4==4.12.3 \
    lxml==5.3.0 \
    PyYAML==6.0.2 \
    python-dateutil==2.9.0.post0 \
 && groupadd --system exporter \
 && useradd --system --gid exporter exporter \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/prometheus-universal-exporter /bin/prometheus-universal-exporter
USER exporter
EXPOSE 8080
ENTRYPOINT ["/bin/prometheus-universal-exporter"]
CMD ["--config.file=/etc/prometheus-universal-exporter/config.yaml", "--web.listen-address=:8080"]
