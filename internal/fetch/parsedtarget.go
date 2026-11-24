//go:build !select_request_types || request_type_icmp || request_type_ntp || request_type_snmp || request_type_amqp || request_type_nats || request_type_radius || request_type_whois

package fetch

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// targetSyntax is how a request type reads its target when it needs nothing
// of the collector to do so. label returns the url label of a request of
// target, or why target is not a target of the type.
type targetSyntax interface {
	label(target string) (string, error)
}

// parsedTarget gives a request type whose target S reads its CheckTarget,
// Label and Display, so that the type's requester embeds it rather than
// writing the three methods.
type parsedTarget[S targetSyntax] struct{}

// CheckTarget checks a collector's target: it is one when S reads it.
func (parsedTarget[S]) CheckTarget(_ *model.Collector, target string, _ bool) error {
	var syntax S
	_, err := syntax.label(target)
	return err
}

// Label is the url label of a request of target.
func (parsedTarget[S]) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	var syntax S
	return syntax.label(target)
}

// Display renders a target for logs and labels: as written, since it holds
// no credentials, when it is a target at all.
func (parsedTarget[S]) Display(target string) string {
	var syntax S
	_, err := syntax.label(target)
	return shownTarget(target, err)
}
