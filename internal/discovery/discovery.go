// Package discovery is what the target allocator (internal/allocator) and
// the exporter's static target file (internal/config) share of discovering
// targets: a target a source found, the target and the name it makes for a
// discovered_targets entry, the labels it keeps, and the dns_sd source,
// which both run. The allocator's other sources are its own.
package discovery

import (
	"fmt"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Target is one target a source found: its address and its labels, the
// source's own and those its labels mapping set.
type Target struct {
	Address string            `json:"address"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// AddressPlaceholder stands for a discovered target's address in a
// target_template.
const AddressPlaceholder = "{{address}}"

// DefaultTimeout bounds an ask of a source, or its refresh_interval when
// that is shorter.
const DefaultTimeout = 10 * time.Second

// ReservedTargetLabels are what a discovery source's labels may not set:
// job is the scrape configuration's, instance the allocator's (a probe's
// target) or Prometheus's, and static_target the static targets endpoint's.
var ReservedTargetLabels = map[string]bool{"job": true, "instance": true, "static_target": true}

// MapLabels is labels with mapping applied: each label mapping names set
// to the value of the source label it names, when that is not empty.
func MapLabels(labels, mapping map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels)+len(mapping))
	for name, value := range labels {
		out[name] = value
	}
	for name, from := range mapping {
		if value := labels[from]; value != "" {
			out[name] = value
		}
	}
	return out
}

// StaticLabels are the labels of a discovered static target that it
// keeps: those the exporter takes on a static target, none starting with
// __, which only relabeling, absent here, would read.
func StaticLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range labels {
		if value == "" || strings.HasPrefix(name, "__") || ReservedTargetLabels[name] || model.CheckLabelName(name) != nil {
			continue
		}
		out[name] = value
	}
	return out
}

// CheckTimeout refuses a timeout set longer than refresh, the interval the
// source is asked at, which refreshText names.
func CheckTimeout(kind string, set model.Duration, refresh time.Duration, refreshText string) error {
	switch {
	case set < 0:
		return fmt.Errorf("%s.timeout must not be negative", kind)
	case time.Duration(set) > refresh:
		return fmt.Errorf("%s.timeout %s is longer than %s", kind, time.Duration(set), refreshText)
	}
	return nil
}

// CheckTemplate refuses a target_template without the address.
func CheckTemplate(template string) error {
	if template != "" && !strings.Contains(template, AddressPlaceholder) {
		return fmt.Errorf("target_template %q has no %s, which stands for each discovered target's address", template, AddressPlaceholder)
	}
	return nil
}

// FromTemplate is the target of the discovered address under template.
func FromTemplate(template, address string) string {
	if template == "" {
		return address
	}
	return strings.ReplaceAll(template, AddressPlaceholder, address)
}
