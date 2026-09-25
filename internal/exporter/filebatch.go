package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A localfile collector reading a directory (fetch/localfile_directory.go) hands
// back every file it read rather than one body. Each is decoded, transformed
// and validated on its own, as if it were the only file, and its series are
// labelled with its name. A file that fails at any stage is left out, alone:
// the failure is logged, counted in the collector's self-metrics like any
// other, and reported in the answer itself, where whoever scrapes it will
// look, under the names node_exporter's textfile collector uses:
//
//	localfile_mtime_seconds{file}   when the file was last modified
//	localfile_scrape_error{file}    1 when the file was left out, else 0
//	localfile_files_skipped         matching files beyond request.max_files

// The series a directory read adds to its answer.
const (
	localFileMTimeMetric   = "localfile_mtime_seconds"
	localFileErrorMetric   = "localfile_scrape_error"
	localFileSkippedMetric = "localfile_files_skipped"
	// localFileLabelName labels every series with the file it came from.
	localFileLabelName = "file"
)

var localFileMetricHelp = map[string]string{
	localFileMTimeMetric:   "Unix time the file was last modified.",
	localFileErrorMetric:   "1 when the file could not be read, decoded or transformed and its series were left out, else 0.",
	localFileSkippedMetric: "Files matching request.files that were not read because the directory had more than request.max_files.",
}

// fileFailure is why one file of a directory was left out.
type fileFailure struct {
	stage string
	err   error
}

// collectDirectory turns a directory read into one metric set.
//
// logTarget is the target as logs show it, keyTarget as the failure log
// tells targets apart (failureKey).
func (s *Server) collectDirectory(ctx context.Context, read *fetch.DirectoryRead, c *model.Collector, rec statsRecorder, logTarget, keyTarget string) *model.MetricSet {
	rec.update(func(x *serverStats) { x.lastBytes = read.Bytes })
	// These hold for as long as the directory stays as it is, so they are
	// logged like repeated failures (failurelog.go).
	listingKey, skippedKey := failureKey(c.Name, keyTarget, "\x00listing"), failureKey(c.Name, keyTarget, "\x00skipped")
	if read.Truncated {
		s.tripFailed(ctx, slog.LevelWarn, listingKey, "directory has more entries than one scrape lists; only the first were considered", "listing", nil, "collector", c.Name, "target", logTarget, "directory", read.Path, "listed", read.Listed, "max_files", c.Request.MaxFiles)
	} else {
		s.tripRecovered(ctx, listingKey, "directory is listed whole again", "collector", c.Name, "target", logTarget, "directory", read.Path)
	}
	if len(read.Skipped) > 0 {
		s.tripFailed(ctx, slog.LevelWarn, skippedKey, "directory has more matching files than request.max_files; the rest were skipped", "max_files", nil, "collector", c.Name, "target", logTarget, "directory", read.Path, "matched", read.Matched, "max_files", c.Request.MaxFiles, "skipped", len(read.Skipped), "first_skipped", read.Skipped[0])
	} else {
		s.tripRecovered(ctx, skippedKey, "directory is within request.max_files again", "collector", c.Name, "target", logTarget, "directory", read.Path)
	}
	// One script timer covers every file: the gauge is the Python this probe
	// ran, whichever files ran it.
	scriptCtx, timer := transform.WithScriptTimer(ctx)
	defer recordScriptDuration(rec, timer)

	var order []string
	families := map[string][]model.Metric{}
	typeFrom := map[string]string{}
	failed := map[string]bool{}
	for _, file := range read.Files {
		set, failure := s.collectFile(scriptCtx, file, c, rec, logTarget, keyTarget)
		if failure == nil {
			failure = checkFileFamilies(set, families, typeFrom)
		}
		fileKey := failureKey(c.Name, keyTarget, file.Name)
		if failure != nil {
			failed[file.Name] = true
			s.tripFailed(ctx, slog.LevelWarn, fileKey, "file of a directory failed; its series are left out and the other files' are answered", failure.stage, failure.err, "collector", c.Name, "target", logTarget, "file", file.Name, "stage", failure.stage)
			continue
		}
		s.tripRecovered(ctx, fileKey, "file of a directory recovered", "collector", c.Name, "target", logTarget, "file", file.Name)
		for _, m := range set.Metrics {
			if _, seen := families[m.Name]; !seen {
				order = append(order, m.Name)
				typeFrom[m.Name] = file.Name
			}
			m.Labels = model.CloneLabels(m.Labels)
			m.Labels[localFileLabelName] = file.Name
			families[m.Name] = append(families[m.Name], m)
		}
	}
	out := &model.MetricSet{}
	for _, name := range order {
		out.Metrics = append(out.Metrics, families[name]...)
	}
	synthetic := func(name string, value float64, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Help: localFileMetricHelp[name], Type: model.GaugeMetricType, Value: value, Labels: labels}
	}
	for _, file := range read.Files {
		if !file.ModTime.IsZero() {
			out.Metrics = append(out.Metrics, synthetic(localFileMTimeMetric, float64(file.ModTime.UnixNano())/1e9, map[string]string{localFileLabelName: file.Name}))
		}
	}
	for _, file := range read.Files {
		value := 0.0
		if failed[file.Name] {
			value = 1
		}
		out.Metrics = append(out.Metrics, synthetic(localFileErrorMetric, value, map[string]string{localFileLabelName: file.Name}))
	}
	out.Metrics = append(out.Metrics, synthetic(localFileSkippedMetric, float64(len(read.Skipped)), nil))
	return out
}

// collectFile decodes, transforms and validates one file, counting each stage
// in the collector's self-metrics as a probe of one file would.
func (s *Server) collectFile(ctx context.Context, file fetch.FileRead, c *model.Collector, rec statsRecorder, logTarget, keyTarget string) (*model.MetricSet, *fileFailure) {
	if file.Err != nil {
		if errors.Is(file.Err, model.ErrLimitExceeded) {
			rec.update(func(x *serverStats) { x.limitErrors++ })
		}
		return nil, &fileFailure{"file", file.Err}
	}
	d, err := decode.Decode(file.Response, c)
	if errors.Is(err, model.ErrLimitExceeded) {
		// Past limits.max_metrics, found by the decoder rather than the
		// validation (transform/serieslimit.go).
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return nil, &fileFailure{"validation", err}
	}
	if err != nil {
		rec.update(func(x *serverStats) { x.parseErrors++ })
		return nil, &fileFailure{"decode", err}
	}
	rec.update(func(x *serverStats) { x.decodeOK++ })
	s.noteGraphite(ctx, d, c, rec, logTarget, keyTarget, file.Name)
	set, repaired, err := s.transformRecorded(ctx, d, file.Response, c, rec)
	if errors.Is(err, model.ErrLimitExceeded) {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return nil, &fileFailure{"validation", err}
	}
	if err != nil {
		rec.update(func(x *serverStats) {
			if errors.Is(err, model.ErrMissingValue) {
				x.missing++
			}
			if errors.Is(err, model.ErrScriptFailed) {
				x.scriptErrors++
			}
			x.transformErrors++
		})
		return nil, &fileFailure{"transform", err}
	}
	if set == nil {
		set = &model.MetricSet{}
	}
	s.noteUTF8Repairs(ctx, repaired, rec, c, file.Name, keyTarget+"\x00"+file.Name)
	if err := set.Validate(c.Limits); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		return nil, &fileFailure{"validation", err}
	}
	for _, m := range set.Metrics {
		if _, ok := m.Labels[localFileLabelName]; ok {
			return nil, &fileFailure{"labels", fmt.Errorf("series %s already has a %q label, which a collector reading a directory sets to the file's name", m.Name, localFileLabelName)}
		}
		if _, reserved := localFileMetricHelp[m.Name]; reserved {
			return nil, &fileFailure{"labels", fmt.Errorf("the metric name %s is reserved for the series a collector reading a directory adds itself", m.Name)}
		}
	}
	return set, nil
}

// checkFileFamilies refuses a file whose metric has a different type than
// the same metric in a file read before it: one exposition can declare a
// family only once.
func checkFileFamilies(set *model.MetricSet, families map[string][]model.Metric, typeFrom map[string]string) *fileFailure {
	var conflicts []string
	reported := map[string]bool{}
	for _, m := range set.Metrics {
		if existing, ok := families[m.Name]; ok && existing[0].Type != m.Type && !reported[m.Name] {
			reported[m.Name] = true
			conflicts = append(conflicts, fmt.Sprintf("%s is a %s here but a %s in %s", m.Name, m.Type, existing[0].Type, typeFrom[m.Name]))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return &fileFailure{"merge", fmt.Errorf("%s: a metric has one type across the directory's files", strings.Join(conflicts, "; "))}
}
