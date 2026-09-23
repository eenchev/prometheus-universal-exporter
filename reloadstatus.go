package main

import (
	"sync"
	"time"
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

const (
	reloadFileConfig  = "config"
	reloadFileTargets = "targets"
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

// reloadMetrics renders the reload status, one family at a time.
func (m *ConfigManager) reloadMetrics() []Metric {
	if m.reloads == nil {
		return nil
	}
	m.reloads.mu.Lock()
	defer m.reloads.mu.Unlock()
	var files []string
	for _, file := range []string{reloadFileConfig, reloadFileTargets} {
		if m.reloads.files[file] != nil {
			files = append(files, file)
		}
	}
	var successful, timestamps, reloads []Metric
	for _, file := range files {
		st := m.reloads.files[file]
		up := 0.0
		if st.successful {
			up = 1
		}
		successful = append(successful, Metric{Name: "http_exporter_config_last_reload_successful", Help: exporterMetricHelp["http_exporter_config_last_reload_successful"], Type: GaugeMetricType, Value: up, Labels: map[string]string{"file": file}})
		timestamps = append(timestamps, Metric{Name: "http_exporter_config_last_reload_success_timestamp_seconds", Help: exporterMetricHelp["http_exporter_config_last_reload_success_timestamp_seconds"], Type: GaugeMetricType, Value: scrapeTimestamp(st.lastSuccess), Labels: map[string]string{"file": file}})
		for _, result := range []struct {
			name  string
			count uint64
		}{{"success", st.successes}, {"failure", st.failures}} {
			reloads = append(reloads, Metric{Name: "http_exporter_config_reloads_total", Help: exporterMetricHelp["http_exporter_config_reloads_total"], Type: CounterMetricType, Value: float64(result.count), Labels: map[string]string{"file": file, "result": result.name}})
		}
	}
	return append(append(successful, timestamps...), reloads...)
}
