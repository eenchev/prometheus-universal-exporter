// Package model holds the data the other packages share: the configuration as
// it is written (Config, Collector and the types under them), the scheduled
// target file, and the metrics a probe produces (MetricSet). It has no
// behaviour beyond decoding its own YAML, checking a metric set against its
// limits, and small helpers on these types.
package model
