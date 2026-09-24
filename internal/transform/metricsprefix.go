package transform

import (
	"fmt"
	"regexp"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's metrics_prefix namespaces everything it exports: with
// metrics_prefix: grafana, a metric declared as statuspage_status is exported
// as grafana_statuspage_status. It applies to every transform, including the
// names a Python script emits and the names a prometheus transform passes
// through, and to /probe and OTLP alike. It does not apply to the exporter's
// own http_exporter_* metrics, which describe the exporter, not the target.
//
// The prefix is a name fragment, not a raw string: it must start with a
// letter and consist of letters and digits in segments joined by single
// underscores, and the exporter adds the "_" that joins it to the name. That
// rules out
//
//   - a leading underscore, since names starting with "__" are reserved by
//     Prometheus and a single one reads as a private metric;
//   - a trailing underscore, which with the joining "_" would give a double one;
//   - "__" anywhere, for the same reason;
//   - ":", which Prometheus reserves for recording rules; and
//   - a leading digit, which no metric name may have.
var MetricsPrefixRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*(?:_[a-zA-Z0-9]+)*$`)

// ValidateMetricsPrefix checks a collector's prefix and, when one is set, that
// every metric name the configuration declares still fits
// limits.max_metric_name_length once prefixed. Names produced at scrape time —
// by a Python script or passed through by a prometheus transform — are checked
// against the same limit when the scrape happens.
func ValidateMetricsPrefix(c *model.Collector) error {
	if c.MetricsPrefix == "" {
		return nil
	}
	if !MetricsPrefixRE.MatchString(c.MetricsPrefix) {
		return fmt.Errorf("collector %q has invalid metrics_prefix %q: it must start with a letter and contain only letters and digits, in parts joined by single underscores (for example \"grafana\" or \"vendor_eu\"); it is joined to each metric name with \"_\", so it must not end with one", c.Name, c.MetricsPrefix)
	}
	limit := c.Limits.MaxMetricNameLength
	if limit > 0 && len(c.MetricsPrefix)+2 > limit {
		return fmt.Errorf("collector %q metrics_prefix %q leaves no room for a metric name within limits.max_metric_name_length %d", c.Name, c.MetricsPrefix, limit)
	}
	for _, rule := range c.Metrics {
		if rule.Name == "" {
			continue
		}
		if name := prefixedMetricName(c.MetricsPrefix, rule.Name); limit > 0 && len(name) > limit {
			return fmt.Errorf("collector %q metric %q is exported as %q, which is longer than limits.max_metric_name_length %d", c.Name, rule.Name, name, limit)
		}
	}
	return nil
}

func prefixedMetricName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "_" + name
}

// applyMetricsPrefix renames every metric in the set.
func applyMetricsPrefix(set *model.MetricSet, prefix string) {
	if prefix == "" {
		return
	}
	for i := range set.Metrics {
		set.Metrics[i].Name = prefixedMetricName(prefix, set.Metrics[i].Name)
	}
}
