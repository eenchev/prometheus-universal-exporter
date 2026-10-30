package exporter

import (
	"testing"
	"time"
)

// --web.status-refresh takes off, or whole seconds from 5s to 24h.
func TestParseStatusRefresh(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"off": 0, "5s": 5 * time.Second, "30s": 30 * time.Second, "1m": time.Minute,
		"90s": 90 * time.Second, "1h30m": 90 * time.Minute, "24h": 24 * time.Hour,
	} {
		if got, err := ParseStatusRefresh(value); err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"", "0", "0s", "1s", "4s", "4.999s", "5.5s", "1500ms", "-30s", "24h0m1s", "48h", "Off", "OFF", "never", "30"} {
		if got, err := ParseStatusRefresh(value); err == nil {
			t.Errorf("%q was taken as %v", value, got)
		} else if want := `--web.status-refresh "` + value + `" must be off or a duration in whole seconds from 5s to 24h, such as 30s or 1m`; err.Error() != want {
			t.Errorf("%q: %v", value, err)
		}
	}
}

// An interval is written as Grafana writes it, without the zero units Go's
// would have.
func TestStatusRefreshText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "off", 5 * time.Second: "5s", time.Minute: "1m", 90 * time.Second: "1m30s",
		time.Hour: "1h", time.Hour + time.Second: "1h1s", 24 * time.Hour: "24h",
	} {
		if got := statusRefreshText(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
