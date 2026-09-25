package exporter

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Version is the release reported by --version and http_exporter_build_info.
// main sets it from version, which -X main.version sets at build time.
var Version string

// Revision is the commit set at build time, -X main.revision, for a build
// without VCS information of Go's own, as the image's is.
var Revision string

type buildInformation struct {
	Version, Revision, GoVersion string
	RequestTypes                 []string
}

// BuildVersion returns what this binary reports about its build: the
// version, the VCS revision, the Go release and the request types built in.
// It is worked out once.
var BuildVersion = sync.OnceValue(computeBuildVersion)

func computeBuildVersion() buildInformation {
	out := buildInformation{Version: Version, Revision: "unknown", GoVersion: runtime.Version(), RequestTypes: fetch.BuiltRequestTypes()}
	if info, ok := debug.ReadBuildInfo(); ok {
		if out.Version == "" {
			out.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				out.Revision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" {
					out.Revision += "-modified"
				}
			}
		}
	}
	if out.Version == "" {
		out.Version = "(devel)"
	}
	if out.Revision == "unknown" && Revision != "" {
		out.Revision = Revision
	}
	return out
}

// VersionString is what --version prints.
func VersionString() string {
	b := BuildVersion()
	return fmt.Sprintf("prometheus-universal-exporter version %s (revision %s, %s, request types %s)", b.Version, b.Revision, b.GoVersion, strings.Join(b.RequestTypes, ","))
}

// buildInfoMetric is http_exporter_build_info, 1 with the build as labels, as
// every Prometheus exporter publishes its own.
func buildInfoMetric() model.Metric {
	b := BuildVersion()
	return model.Metric{Name: "http_exporter_build_info", Help: exporterMetricHelp["http_exporter_build_info"], Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{
		"version": b.Version, "revision": b.Revision, "goversion": b.GoVersion, "request_types": strings.Join(b.RequestTypes, ","),
	}}
}
