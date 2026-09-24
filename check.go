package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// --dry-run validates what the exporter would load and exits, without binding a
// port, starting a watch or contacting a single target. It is for CI, for a
// pre-deploy hook and for an init container: the question "would this start?"
// answered without starting it.
//
// Every check below calls the function startup calls for the same step —
// config.Load, transform.CheckPythonScripts, validateWatchInterval,
// config.LoadTargets, config.ValidateTargets and config.ValidateTargetsAgainst —
// so the verdict cannot drift from what a real
// start would do. A check that passes means the same files, with the same
// flags, would start; one that fails names the step and the reason.
//
// The report goes to stdout as one JSON document, so `--dry-run | jq` works and
// a pipeline can read it without parsing log lines. The log lines go to stderr
// as JSON, like every other line the exporter writes, so a log collector
// watching a pre-deploy job sees the same shape it sees from a running pod.

const (
	checkOK      = "ok"
	checkFailed  = "failed"
	checkSkipped = "skipped"
)

// checkInputs are the flags that decide what startup loads and how.
type checkInputs struct {
	ConfigFile    string
	TargetFile    string
	PythonPath    string
	ExpandEnv     bool
	Watch         bool
	WatchInterval time.Duration
}

// checkReport is the document --dry-run prints. Status is "ok" only when every
// check is ok.
type checkReport struct {
	Status string        `json:"status"`
	Checks []checkResult `json:"checks"`
	// RequestTypes are the request types this binary was built with, so a
	// configuration can be checked against the build that will run it.
	RequestTypes []string `json:"request_types"`
}

// checkResult is one step of startup. Errors lists every fault the step found
// — the Python check can find several at once — and Reason says why a step
// that depends on a failed one was not run. Details summarise what an ok step
// loaded, so a passing report still says what it passed.
type checkResult struct {
	Check   string         `json:"check"`
	File    string         `json:"file,omitempty"`
	Status  string         `json:"status"`
	Errors  []string       `json:"errors,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// runCheck performs the checks, logs each, prints the report and returns the
// exit status: 0 when everything is ok and 1 otherwise.
func runCheck(in checkInputs, stdout io.Writer, logger *slog.Logger) int {
	report := checkStartup(in)
	failed := 0
	for _, result := range report.Checks {
		attrs := []any{"check", result.Check}
		if result.File != "" {
			attrs = append(attrs, "file", result.File)
		}
		if deprecations, ok := result.Details["deprecations"].([]string); ok {
			for _, message := range deprecations {
				logger.Warn("deprecated configuration", "file", result.File, "deprecation", message)
			}
		}
		switch result.Status {
		case checkOK:
			logger.Info("configuration check passed", attrs...)
		case checkSkipped:
			failed++
			logger.Warn("configuration check skipped", append(attrs, "reason", result.Reason)...)
		default:
			failed++
			logger.Error("configuration check failed", append(attrs, "errors", result.Errors)...)
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		logger.Error("writing the check report failed", "error", err)
		return 1
	}
	if report.Status != checkOK {
		logger.Error("configuration check complete", "status", report.Status, "checks", len(report.Checks), "not_ok", failed)
		return 1
	}
	logger.Info("configuration check complete", "status", report.Status, "checks", len(report.Checks), "not_ok", 0)
	return 0
}

// checkStartup runs startup's validation steps in startup's order. A step
// whose input failed to load is reported as skipped rather than silently
// omitted, so a report never looks shorter because something went wrong.
func checkStartup(in checkInputs) checkReport {
	var options []config.LoadOption
	if in.ExpandEnv {
		options = append(options, config.WithEnvExpansion())
	}
	var results []checkResult

	conf, err := config.Load(in.ConfigFile, options...)
	if err != nil {
		results = append(results, failedCheck("config", in.ConfigFile, err))
	} else {
		names := make([]string, 0, len(conf.Collectors))
		for _, c := range conf.Collectors {
			names = append(names, c.Name)
		}
		details := map[string]any{
			"collectors":        names,
			"otlp_enabled":      conf.OTLP.Enabled,
			"config_export_env": in.ExpandEnv,
		}
		// A deprecated spelling still passes, so the check stays ok; the
		// report says what to change before the spelling is removed.
		if len(conf.LoadedCollectorFiles) > 0 {
			details["collector_files"] = conf.LoadedCollectorFiles
		}
		if len(conf.Deprecations) > 0 {
			details["deprecations"] = conf.Deprecations
		}
		results = append(results, checkResult{Check: "config", File: in.ConfigFile, Status: checkOK, Details: details})
	}

	if conf == nil {
		results = append(results, skippedCheck("python_scripts", in.ConfigFile, "the configuration did not load, so its scripts could not be read"))
	} else {
		scripts := len(transform.CollectorScripts(conf))
		problems, err := transform.CheckPythonScripts(in.PythonPath, conf)
		switch {
		case err != nil:
			results = append(results, failedCheck("python_scripts", in.ConfigFile, err))
		case len(problems) > 0:
			results = append(results, checkResult{Check: "python_scripts", File: in.ConfigFile, Status: checkFailed, Errors: problems})
		default:
			details := map[string]any{"scripts": scripts}
			if scripts > 0 {
				details["python_path"] = in.PythonPath
			}
			results = append(results, checkResult{Check: "python_scripts", File: in.ConfigFile, Status: checkOK, Details: details})
		}
	}

	// The watch flags are only checked when the watch is asked for, exactly as
	// at startup, where an interval without --config.watch is never read.
	if in.Watch {
		if err := validateWatchInterval(in.Watch, in.WatchInterval); err != nil {
			results = append(results, failedCheck("config_watch", "", err))
		} else {
			results = append(results, checkResult{Check: "config_watch", Status: checkOK, Details: map[string]any{"interval": in.WatchInterval.String()}})
		}
	}

	if in.TargetFile != "" {
		results = append(results, checkTargets(in.TargetFile, options, conf))
	}

	status := checkOK
	for _, result := range results {
		if result.Status != checkOK {
			status = checkFailed
		}
	}
	return checkReport{Status: status, Checks: results, RequestTypes: fetch.BuiltRequestTypes()}
}

// checkTargets validates the scheduled target document on its own and then
// against the configuration. The first half needs no configuration, so a
// broken target file is still reported when the configuration is broken too.
func checkTargets(path string, options []config.LoadOption, conf *model.Config) checkResult {
	file, err := config.LoadTargets(path, options...)
	if err == nil {
		err = config.ValidateTargets(file)
	}
	if err != nil {
		return failedCheck("targets", path, err)
	}
	names := make([]string, 0, len(file.Targets))
	for _, target := range file.Targets {
		names = append(names, target.Name)
	}
	details := map[string]any{"targets": names}
	if conf == nil {
		result := skippedCheck("targets", path, "the file is valid on its own, but could not be checked against the configuration, which did not load")
		result.Details = details
		return result
	}
	if err := config.ValidateTargetsAgainst(file, conf); err != nil {
		result := failedCheck("targets", path, err)
		result.Details = details
		return result
	}
	return checkResult{Check: "targets", File: path, Status: checkOK, Details: details}
}

func failedCheck(check, file string, err error) checkResult {
	return checkResult{Check: check, File: file, Status: checkFailed, Errors: []string{err.Error()}}
}

func skippedCheck(check, file, reason string) checkResult {
	return checkResult{Check: check, File: file, Status: checkSkipped, Reason: reason}
}

// validateWatchInterval is shared by startup and --dry-run. A non-positive
// interval with the watch on is a startup error rather than a silently
// disabled watch that was explicitly requested.
func validateWatchInterval(watch bool, interval time.Duration) error {
	if watch && interval <= 0 {
		return fmt.Errorf("config.watch-interval must be positive, got %s", interval)
	}
	return nil
}
