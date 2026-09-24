package config

import (
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A reload that is rejected leaves the last valid configuration in force,
// which is the right thing for the exporter to do and the wrong thing to keep
// quiet about: nothing changes, so nothing looks wrong, while the change
// somebody made is not running. The log says so once. These self-metrics say
// so for as long as it is true, so it can be alerted on, under the names
// Prometheus itself uses for the same thing:
//
//	http_exporter_config_last_reload_successful{file}
//	http_exporter_config_last_reload_success_timestamp_seconds{file}
//	http_exporter_config_reloads_total{file, result}
//
// file is "config" for the configuration file, with its collector files, and
// "targets" for the scheduled target file, which is only reported when one is
// configured. Loading at startup counts as a successful load; the counter
// counts only reloads after it.

// ReloadMetricHelp is the help of the reload families.
var ReloadMetricHelp = map[string]string{
	"http_exporter_config_last_reload_successful":                "Whether the last load of this configuration file, at startup or on reload, succeeded.",
	"http_exporter_config_last_reload_success_timestamp_seconds": "Unix time this configuration file was last loaded successfully, at startup or on reload.",
	"http_exporter_config_reloads_total":                         "Reloads of this configuration file after startup, by result: success or failure.",
}

// The files a reload status is kept for: the configuration file, with its
// collector files, and the scheduled target file.
const (
	reloadFileConfig  = "config"
	ReloadFileTargets = "targets"
)

type reloadFileStatus struct {
	successful          bool
	lastSuccess         time.Time
	successes, failures uint64
}

type reloadStatus struct {
	mu    sync.Mutex
	files map[string]*reloadFileStatus
}

func newReloadStatus() *reloadStatus {
	return &reloadStatus{files: map[string]*reloadFileStatus{}}
}

// loaded records the load at startup.
func (r *reloadStatus) loaded(file string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[file] = &reloadFileStatus{successful: true, lastSuccess: time.Now()}
}

// record records a reload and whether it was applied.
func (r *reloadStatus) record(file string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.files[file]
	if st == nil {
		st = &reloadFileStatus{}
		r.files[file] = st
	}
	st.successful = ok
	if ok {
		st.lastSuccess = time.Now()
		st.successes++
		return
	}
	st.failures++
}

// ReloadMetrics renders the reload status, one family at a time.
func (m *Manager) ReloadMetrics() []model.Metric {
	if m.Reloads == nil {
		return nil
	}
	m.Reloads.mu.Lock()
	defer m.Reloads.mu.Unlock()
	var files []string
	for _, file := range []string{reloadFileConfig, ReloadFileTargets} {
		if m.Reloads.files[file] != nil {
			files = append(files, file)
		}
	}
	var successful, timestamps, reloads []model.Metric
	for _, file := range files {
		st := m.Reloads.files[file]
		up := 0.0
		if st.successful {
			up = 1
		}
		successful = append(successful, model.Metric{Name: "http_exporter_config_last_reload_successful", Help: ReloadMetricHelp["http_exporter_config_last_reload_successful"], Type: model.GaugeMetricType, Value: up, Labels: map[string]string{"file": file}})
		timestamps = append(timestamps, model.Metric{Name: "http_exporter_config_last_reload_success_timestamp_seconds", Help: ReloadMetricHelp["http_exporter_config_last_reload_success_timestamp_seconds"], Type: model.GaugeMetricType, Value: model.ScrapeTimestamp(st.lastSuccess), Labels: map[string]string{"file": file}})
		for _, result := range []struct {
			name  string
			count uint64
		}{{"success", st.successes}, {"failure", st.failures}} {
			reloads = append(reloads, model.Metric{Name: "http_exporter_config_reloads_total", Help: ReloadMetricHelp["http_exporter_config_reloads_total"], Type: model.CounterMetricType, Value: float64(result.count), Labels: map[string]string{"file": file, "result": result.name}})
		}
	}
	return append(append(successful, timestamps...), reloads...)
}

// Rejected lists the files whose last reload was rejected.
func (r *reloadStatus) Rejected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, file := range []string{reloadFileConfig, ReloadFileTargets} {
		if st := r.files[file]; st != nil && !st.successful {
			out = append(out, file)
		}
	}
	return out
}
