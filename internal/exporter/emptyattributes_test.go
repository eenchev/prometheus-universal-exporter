package exporter

import (
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter exports no label, and no OTLP attribute, with an empty
// value. Besides the labels of a collector's series, two things of its own
// had one: an OTLP resource attribute written "", and the target label of
// the health series of a static target that names no target.

// targetResourceAsItWas is targetResource, and with a target that sets
// nothing defaultResourceIdentity, as they were while an attribute written
// "" was one of the resource's, kept as the oracle.
func targetResourceAsItWas(t *model.StaticTarget, cfg model.OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		identity.Attributes[key] = value
	}
	if t.OTLP.ServiceName != "" {
		identity.ServiceName = t.OTLP.ServiceName
	}
	for key, value := range t.OTLP.ResourceAttributes {
		identity.Attributes[key] = value
	}
	return identity
}

// staticTargetHealthMetricsAsItWas is staticTargetHealthMetrics as it was
// while a target that names none had the label target="", kept as the
// oracle.
func staticTargetHealthMetricsAsItWas(target model.StaticTarget, c *model.Collector, up, duration float64, lastSuccess time.Time) model.MetricSet {
	labels := map[string]string{"collector": c.Name, "static_target": target.Name, "target": fetch.DisplayTarget(c, target.Target)}
	for name, value := range target.Labels {
		if _, exists := labels[name]; !exists && value != "" {
			labels[name] = value
		}
	}
	return model.MetricSet{Metrics: []model.Metric{
		{Name: "http_exporter_target_up", Help: "Whether the last scrape of this static target succeeded.", Type: model.GaugeMetricType, Value: up, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_scrape_duration_seconds", Help: "Duration of the last scrape of this static target in seconds.", Type: model.GaugeMetricType, Value: duration, Labels: model.CloneLabels(labels)},
		{Name: "http_exporter_target_last_success_timestamp_seconds", Help: "Unix time of the last successful scrape of this static target; 0 if none has succeeded.", Type: model.GaugeMetricType, Value: unixSeconds(lastSuccess), Labels: model.CloneLabels(labels)},
	}}
}

// An OTLP resource attribute written "" is the attribute left out, as a
// transform.labels value written "" is the label left out: the
// exporter-wide resource does not carry it, and a static target's sets
// nothing, so that an exporter-wide attribute of the name stays where a
// value that is not empty replaces it. The resource was exported with an
// attribute of no value, and a target's "" emptied the exporter-wide one.
func TestAnOTLPResourceAttributeWrittenEmptyIsLeftOut(t *testing.T) {
	cfg := model.OTLPConfig{ServiceName: "exporter", ResourceAttributes: map[string]string{"deployment.environment": "test", "service.namespace": "", "region": "eu"}}
	if got, want := defaultResourceIdentity(cfg).Attributes, map[string]string{"deployment.environment": "test", "region": "eu"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the exporter-wide resource has the attributes %q, want %q", got, want)
	}
	target := &model.StaticTarget{Name: "eu"}
	target.OTLP.ServiceName, target.OTLP.ResourceAttributes = "legacy", map[string]string{"region": "", "team": "", "deployment.environment": "prod", "zone": "a"}
	identity := targetResource(target, cfg)
	if want := map[string]string{"deployment.environment": "prod", "region": "eu", "zone": "a"}; identity.ServiceName != "legacy" || !reflect.DeepEqual(identity.Attributes, want) {
		t.Errorf("the target's resource is %q with the attributes %q, want legacy with %q", identity.ServiceName, identity.Attributes, want)
	}
	for _, attribute := range identity.attributes() {
		if attribute.Value.StringValue == "" {
			t.Errorf("the resource carries %s without a value", attribute.Key)
		}
	}
	// A resource is known by its attributes: two targets that differ only
	// in an attribute written "" share one.
	other := &model.StaticTarget{Name: "us"}
	other.OTLP.ServiceName, other.OTLP.ResourceAttributes = "legacy", map[string]string{"deployment.environment": "prod", "zone": "a"}
	if identity.key() != targetResource(other, cfg).key() {
		t.Errorf("the resources %q and %q are told apart", identity.key(), targetResource(other, cfg).key())
	}
}

// Leaving an empty resource attribute out changes nothing else: over a
// table of exporter-wide and per-target attributes, a resource is the one
// it was when no attribute is empty, and otherwise the one it was from the
// same attributes without the empty ones, for a target and for the
// exporter-wide resource alike.
func TestOnlyAnEmptyResourceAttributeIsReadAnew(t *testing.T) {
	sets := []map[string]string{
		nil, {}, {"env": "test"}, {"env": ""}, {"env": " "}, {"env": "test", "region": ""}, {"env": "", "region": ""}, {"env": "prod", "region": "eu", "zone": "a"}, {"zone": ""}, {"zone": "b"},
	}
	without := func(attributes map[string]string) map[string]string {
		if attributes == nil {
			return nil
		}
		out := map[string]string{}
		for name, value := range attributes {
			if value != "" {
				out[name] = value
			}
		}
		return out
	}
	tried, empty := 0, 0
	for _, wide := range sets {
		for _, own := range sets {
			for _, name := range []string{"", "legacy"} {
				tried++
				cfg := model.OTLPConfig{ServiceName: "exporter", ResourceAttributes: wide}
				target := &model.StaticTarget{Name: "eu"}
				target.OTLP.ServiceName, target.OTLP.ResourceAttributes = name, own
				asItWas := model.OTLPConfig{ServiceName: "exporter", ResourceAttributes: without(wide)}
				targetAsItWas := &model.StaticTarget{Name: "eu"}
				targetAsItWas.OTLP.ServiceName, targetAsItWas.OTLP.ResourceAttributes = name, without(own)
				if len(without(wide)) != len(wide) || len(without(own)) != len(own) {
					empty++
				}
				if got, want := targetResource(target, cfg), targetResourceAsItWas(targetAsItWas, asItWas); !reflect.DeepEqual(got, want) {
					t.Errorf("a target with %q under %q has the resource %+v, want %+v", own, wide, got, want)
				}
				if got, want := defaultResourceIdentity(cfg), targetResourceAsItWas(&model.StaticTarget{}, asItWas); !reflect.DeepEqual(got, want) {
					t.Errorf("the exporter-wide resource of %q is %+v, want %+v", wide, got, want)
				}
			}
		}
	}
	if tried < 150 || empty < 60 || empty > tried-30 {
		t.Fatalf("%d pairs were tried, %d of them with an empty attribute", tried, empty)
	}
}

// The health series of a static target that names no target have no target
// label: they had target="", which to Prometheus is no label. A target
// that names one has the label as it had, the target's own labels are
// added as they were, and a label of the target's named target is still
// not the health series', with a target address or without.
func TestTheHealthSeriesOfAStaticTargetWithoutATargetHaveNoTargetLabel(t *testing.T) {
	c := &model.Collector{Name: "text"}
	tried, without := 0, 0
	for _, address := range []string{"", "http://a.example", "http://user:secret@a.example/x?token=t", "a.example:8080"} {
		for _, labels := range []map[string]string{nil, {"team": "core"}, {"team": "", "zone": "a"}, {"target": "mine", "zone": "a"}, {"target": "", "collector": "other"}} {
			tried++
			target := model.StaticTarget{Name: "eu", Target: address, Labels: labels}
			got := staticTargetHealthMetrics(target, c, 1, 0.5, time.Unix(1700000000, 0))
			want := staticTargetHealthMetricsAsItWas(target, c, 1, 0.5, time.Unix(1700000000, 0))
			if len(got.Metrics) != 3 {
				t.Fatalf("%d health series", len(got.Metrics))
			}
			for i := range want.Metrics {
				if shown, has := want.Metrics[i].Labels["target"]; has && shown == "" {
					delete(want.Metrics[i].Labels, "target")
					without++
				}
				for name, value := range got.Metrics[i].Labels {
					if value == "" {
						t.Errorf("%q with %q: %s has the label %s empty", address, labels, got.Metrics[i].Name, name)
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q with %q: the health series are %+v, want %+v", address, labels, got, want)
			}
			if _, has := got.Metrics[0].Labels["target"]; has != (address != "") {
				t.Errorf("%q with %q: the health series have the labels %q", address, labels, got.Metrics[0].Labels)
			}
		}
	}
	if tried != 20 || without != 15 {
		t.Fatalf("%d targets were tried, and %d health series were without a target", tried, without)
	}
}
