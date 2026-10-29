package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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
// config.Load, config.CheckPythonScripts, validateWatchInterval,
// config.LoadStaticTargets, config.ValidateStaticTargets and config.ValidateStaticTargetsAgainst —
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
	ConfigFile string
	// ConfigError, when set, fails the configuration check without reading
	// ConfigFile: in allocator mode, a config.yaml in the cache directory
	// that is not a bundle the client installed, which a start ignores.
	ConfigError      error
	StaticTargetFile string
	PythonPath       string
	ExpandEnv        bool
	ExpandTargetsEnv bool
	Watch            bool
	WatchInterval    time.Duration
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
		logNotices(logger, result)
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

	var conf *model.Config
	err := in.ConfigError
	if err == nil {
		conf, err = config.Load(in.ConfigFile, options...)
	}
	if err != nil {
		results = append(results, failedCheck("config", in.ConfigFile, err))
	} else {
		results = append(results, checkResult{Check: "config", File: in.ConfigFile, Status: checkOK, Details: configDetails(conf, in.ExpandEnv)})
	}

	if conf == nil {
		results = append(results, skippedCheck("python_scripts", in.ConfigFile, "the configuration did not load, so its scripts could not be read"))
	} else {
		scripts := len(transform.CollectorScripts(conf))
		problems, err := config.CheckPythonScripts(in.PythonPath, conf)
		switch {
		case err != nil:
			results = append(results, failedCheck("python_scripts", in.ConfigFile, err))
		case len(problems) > 0:
			results = append(results, failedCheck("python_scripts", in.ConfigFile, model.Problems(problems)))
		default:
			details := map[string]any{"scripts": scripts}
			if scripts > 0 {
				details["python_path"] = in.PythonPath
			}
			results = append(results, checkResult{Check: "python_scripts", File: in.ConfigFile, Status: checkOK, Details: details})
		}
	}

	// The watch is reported only when it is asked for, exactly as at
	// startup, where an interval without --config.watch is never read. A
	// non-positive interval never gets here: it is a command-line error
	// (exit 2) before the check, as at startup.
	if in.Watch {
		results = append(results, checkResult{Check: "config_watch", Status: checkOK, Details: map[string]any{"interval": in.WatchInterval.String()}})
	}

	if in.StaticTargetFile != "" {
		var targetOptions []config.LoadOption
		if in.ExpandTargetsEnv {
			targetOptions = append(targetOptions, config.WithStaticTargetsEnvExpansion())
		}
		results = append(results, checkTargets(in.StaticTargetFile, targetOptions, in.ExpandTargetsEnv, conf))
	}

	status := checkOK
	for _, result := range results {
		if result.Status != checkOK {
			status = checkFailed
		}
	}
	return checkReport{Status: status, Checks: results, RequestTypes: fetch.BuiltRequestTypes()}
}

// requestPolicy is what a collector's request lets through: the targets it
// may and may not reach, the statuses it decodes and those it retries,
// whether a retry waits as the target's Retry-After asks, and how it
// connects, over a Unix socket or through which proxy, as they will be
// applied, so a list that says less or more than meant, or a proxy taken
// from where it was not meant to come from, shows before it is deployed.
type requestPolicy struct {
	AllowedTargets []string `json:"allowed_targets,omitempty"`
	DeniedTargets  []string `json:"denied_targets,omitempty"`
	AcceptStatus   []string `json:"accept_status,omitempty"`
	AcceptCodes    []string `json:"accept_codes,omitempty"`
	// RetryStatuses is request.retry.statuses when the collector sets it,
	// which replaces the default set: an empty list, which retries no
	// status, is shown as [] rather than left out.
	RetryStatuses *[]string `json:"retry_statuses,omitempty"`
	// RetryAfter is request.retry.retry_after: a retried answer's
	// Retry-After replaces the backoff, which changes how long a trip
	// waits on what the target says.
	RetryAfter bool `json:"retry_after,omitempty"`
	// Pagination is request.pagination as it will be applied, its defaults
	// filled in: max_pages above all, which bounds what one trip asks of
	// the target.
	Pagination *model.PaginationConfig `json:"pagination,omitempty"`
	// Login is request.login as it will be applied: where it goes, where
	// the token is read from and how it is sent, its header and prefix
	// filled in, and the files its secrets are read from, never what they
	// hold; a field or endpoint parameter whose name reads as a
	// credential's is shown without its value.
	Login *model.LoginConfig `json:"login,omitempty"`
	// RateLimit is request.rate_limit as it will be applied, its burst
	// filled in: how fast the collector asks each origin of its targets.
	RateLimit *model.RateLimitConfig `json:"rate_limit,omitempty"`
	// UnixSocket is an http collector's request.unix_socket, which its
	// requests are sent over in place of a TCP connection, and in place of
	// any proxy, so Proxy is left out beside it.
	UnixSocket string `json:"unix_socket,omitempty"`
	// Proxy is how a collector whose requests may go through a proxy
	// reaches its targets (requestProxy): the proxy_url in force, its own
	// or its file's defaults', direct, or environment.
	Proxy string `json:"proxy,omitempty"`
	// ProxyBasicAuthFile is request.proxy_basic_auth_file: the files the
	// proxy's credentials are read from, never what they hold, as a
	// login's are shown.
	ProxyBasicAuthFile *model.ProxyBasicAuthFile `json:"proxy_basic_auth_file,omitempty"`
}

// proxyFromEnvironment is requestPolicy.Proxy of a collector that names no
// proxy: its requests go through the proxy HTTP_PROXY or HTTPS_PROXY names,
// for the target's scheme, but to a host NO_PROXY lists, as the
// environment says when the exporter starts (fetch's environmentProxy). The
// environment's values are not shown: a proxy URL there may hold a
// password, which --dry-run never prints.
const proxyFromEnvironment = "environment"

// requestProxy is requestPolicy.Proxy of c, which the transport goes by
// (fetch.transportProxy): "" for a collector whose type takes no proxy or
// that sends its requests to a Unix socket, proxyFromEnvironment for one
// that names none, and else its proxy_url, direct or a proxy's URL, which
// the load holds to a scheme, a host and a port without credentials, so it
// is shown as written.
func requestProxy(c *model.Collector, socket string) string {
	if rt := fetch.RequestTypes[c.Request.Type]; rt == nil || !rt.Takes("proxy_url") || socket != "" {
		return ""
	}
	if c.Request.ProxyURL == "" {
		return proxyFromEnvironment
	}
	return c.Request.ProxyURL
}

// shownLogin is l as --dry-run shows it (requestPolicy.Login).
func shownLogin(l *model.LoginConfig) *model.LoginConfig {
	if l == nil {
		return nil
	}
	shown := *l
	masked := func(values map[string]string) map[string]string {
		if values == nil {
			return nil
		}
		out := make(map[string]string, len(values))
		for name, value := range values {
			if fetch.CredentialName(name) {
				value = fetch.Redacted
			}
			out[name] = value
		}
		return out
	}
	shown.Fields, shown.EndpointParams = masked(l.Fields), masked(l.EndpointParams)
	shown.Header = fetch.LoginHeader(l)
	if l.TokenCookie == "" {
		prefix := fetch.LoginPrefix(l)
		shown.Prefix = &prefix
	}
	return &shown
}

// requestPolicies are the policies of the collectors that set one, by name.
func requestPolicies(conf *model.Config) map[string]requestPolicy {
	out := map[string]requestPolicy{}
	for i := range conf.Collectors {
		c := &conf.Collectors[i]
		own := fetch.CollectorPolicy(c)
		p := requestPolicy{AllowedTargets: c.Request.AllowedTargets, DeniedTargets: c.Request.DeniedTargets, AcceptStatus: c.Request.AcceptStatus, AcceptCodes: own.AcceptCodes, Pagination: own.Pagination, Login: shownLogin(c.Request.Login), RateLimit: c.Request.RateLimit}
		if retried := c.Request.Retry.Statuses; retried != nil {
			p.RetryStatuses = &retried
		}
		p.RetryAfter = c.Request.Retry.RetryAfter
		p.UnixSocket, p.Proxy, p.ProxyBasicAuthFile = own.UnixSocket, requestProxy(c, own.UnixSocket), c.Request.ProxyBasicAuthFile
		if len(p.AllowedTargets)+len(p.DeniedTargets)+len(p.AcceptStatus)+len(p.AcceptCodes) > 0 || p.Pagination != nil || p.RetryStatuses != nil || p.RetryAfter || p.Login != nil || p.RateLimit != nil || p.UnixSocket != "" || p.Proxy != "" {
			out[c.Name] = p
		}
	}
	return out
}

// defaultsTaken are the keys each collector that took any from its file's
// defaults block took, by collector name.
func defaultsTaken(conf *model.Config) map[string][]string {
	out := map[string][]string{}
	for i := range conf.Collectors {
		if c := &conf.Collectors[i]; len(c.FromDefaults) > 0 {
			out[c.Name] = c.FromDefaults
		}
	}
	return out
}

// configDetails summarises a configuration that loaded. A deprecated spelling,
// a warning or the metric lint's advice still passes, so the check stays ok; the report lists them so
// the operator knows what to change before a deprecated spelling is removed.
// The warnings are the configuration's, and what reading the files a request
// reads, which loading leaves to the request, finds wrong with them now
// (fetch.FileWarnings): those are the check's alone, not startup's.
func configDetails(conf *model.Config, expandEnv bool) map[string]any {
	names := make([]string, 0, len(conf.Collectors))
	for i := range conf.Collectors {
		names = append(names, conf.Collectors[i].Name)
	}
	details := map[string]any{
		"collectors":        names,
		"otlp_enabled":      conf.OTLP.Enabled,
		"config_expand_env": expandEnv,
	}
	if len(conf.LoadedCollectorFiles) > 0 {
		details["collector_files"] = conf.LoadedCollectorFiles
	}
	if policies := requestPolicies(conf); len(policies) > 0 {
		details["request_policies"] = policies
	}
	// The values themselves are the collector's, wherever they are written;
	// which keys it took from its file's defaults says where to change one.
	if taken := defaultsTaken(conf); len(taken) > 0 {
		details["collector_defaults"] = taken
	}
	if len(conf.Deprecations) > 0 {
		details["deprecations"] = conf.Deprecations
	}
	warnings := slices.Clone(conf.Warnings)
	for i := range conf.Collectors {
		warnings = append(warnings, fetch.FileWarnings(&conf.Collectors[i], nil)...)
	}
	if len(warnings) > 0 {
		details["warnings"] = warnings
	}
	// The metric lint is advice, not a fault: it is listed apart from the
	// warnings, each finding by collector and rule so that a pipeline can
	// pick out what it cares about, and never fails the check.
	if findings := transform.LintConfiguration(conf); len(findings) > 0 {
		details["lint"] = findings
	}
	return details
}

// logNotices logs the deprecations and warnings a check's details list, as
// startup logs them.
func logNotices(logger *slog.Logger, result checkResult) {
	if deprecations, ok := result.Details["deprecations"].([]string); ok {
		for _, message := range deprecations {
			logger.Warn("deprecated configuration", "file", result.File, "deprecation", message)
		}
	}
	if warnings, ok := result.Details["warnings"].([]string); ok {
		for _, message := range warnings {
			logger.Warn("configuration warning", "file", result.File, "warning", message)
		}
	}
	if findings, ok := result.Details["lint"].([]transform.LintFinding); ok {
		for _, f := range findings {
			logger.Warn("metric lint", "file", result.File, "collector", f.Collector, "rule", f.Rule, "metric", f.Metric, "advice", f.Message)
		}
	}
}

// checkTargets validates the static target document on its own and then
// against the configuration. The first half needs no configuration, so a
// broken target file is still reported when the configuration is broken too.
func checkTargets(path string, options []config.LoadOption, expandEnv bool, conf *model.Config) checkResult {
	file, err := config.LoadStaticTargets(path, options...)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err != nil {
		return failedCheck("static_targets", path, err)
	}
	names := make([]string, 0, len(file.Targets))
	for _, target := range file.Targets {
		names = append(names, target.Name)
	}
	details := map[string]any{"targets": names, "static_targets_expand_env": expandEnv}
	if conf == nil {
		result := skippedCheck("static_targets", path, "the file is valid on its own, but could not be checked against the configuration, which did not load")
		result.Details = details
		return result
	}
	if err := config.ValidateStaticTargetsAgainst(file, conf); err != nil {
		result := failedCheck("static_targets", path, err)
		result.Details = details
		return result
	}
	// The files a request reads, which loading leaves to the request, are
	// read now as a request would read them: what is wrong with one is a
	// warning, since the file may be put in place after the exporter starts.
	var warnings []string
	for i := range file.Targets {
		target := &file.Targets[i]
		for j := range conf.Collectors {
			if c := &conf.Collectors[j]; c.Name == target.Collector {
				warnings = append(warnings, fetch.FileWarnings(c, target)...)
			}
		}
	}
	if len(warnings) > 0 {
		details["warnings"] = warnings
	}
	return checkResult{Check: "static_targets", File: path, Status: checkOK, Details: details}
}

// failedCheck lists each problem of err (model.Problems) as an error of its
// own, all of them, where the error's text counts those past the first 20.
func failedCheck(check, file string, err error) checkResult {
	var errs []string
	var problems model.Problems
	if errors.As(err, &problems) {
		for _, problem := range problems {
			errs = append(errs, problem.Error())
		}
	} else {
		errs = []string{err.Error()}
	}
	return checkResult{Check: check, File: file, Status: checkFailed, Errors: errs}
}

func skippedCheck(check, file, reason string) checkResult {
	return checkResult{Check: check, File: file, Status: checkSkipped, Reason: reason}
}

// validateWatchInterval checks the watch flags with the other flags, before
// startup or --dry-run. A non-positive interval with the watch on is a
// command-line error rather than a silently disabled watch that was
// explicitly requested.
func validateWatchInterval(watch bool, interval time.Duration) error {
	if watch && interval <= 0 {
		return fmt.Errorf("--config.watch-interval must be positive with --config.watch, got %s", interval)
	}
	return nil
}
