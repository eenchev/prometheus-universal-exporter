package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// version is the release, set at build time with
//
//	go build -ldflags "-X main.version=1.4.0"
//
// Left empty, the module version Go stamps into the binary is used: the tag
// of a build from a tagged checkout, or "(devel)". The revision is always
// the commit Go stamps, when it built from a git checkout.
var version string

type buildInformation struct {
	Version, Revision, GoVersion string
	RequestTypes                 []string
}

var buildVersion = sync.OnceValue(computeBuildVersion)

func computeBuildVersion() buildInformation {
	out := buildInformation{Version: version, Revision: "unknown", GoVersion: runtime.Version(), RequestTypes: builtRequestTypes()}
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
	return out
}

// versionString is what --version prints.
func versionString() string {
	b := buildVersion()
	return fmt.Sprintf("prometheus-universal-exporter version %s (revision %s, %s, request types %s)", b.Version, b.Revision, b.GoVersion, strings.Join(b.RequestTypes, ","))
}

// buildInfoMetric is http_exporter_build_info, 1 with the build as labels, as
// every Prometheus exporter publishes its own.
func buildInfoMetric() Metric {
	b := buildVersion()
	return Metric{Name: "http_exporter_build_info", Help: exporterMetricHelp["http_exporter_build_info"], Type: GaugeMetricType, Value: 1, Labels: map[string]string{
		"version": b.Version, "revision": b.Revision, "goversion": b.GoVersion, "request_types": strings.Join(b.RequestTypes, ","),
	}}
}
