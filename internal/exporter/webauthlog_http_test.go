//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A password file that cannot be read fails every request, unauthenticated
// ones too, and each logged an error: fifty requests were fifty ERROR lines.
// It is logged as a repeated failure is: one error, the repeats at debug
// level, and one line when the file can be read again.
func TestAnUnreadablePasswordFileIsLoggedOnceAndItsRecoveryToo(t *testing.T) {
	dir := t.TempDir()
	password := testutil.WriteIn(t, dir, "password", "s3cret")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", PasswordFile: password}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", logger)

	if err := os.Remove(password); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		user := ""
		if i%2 == 0 {
			user = "exporter"
		}
		if got := selfMetricsAs(t, server, user, "s3cret"); got != http.StatusInternalServerError {
			t.Fatalf("request %d with the file gone answered %d", i, got)
		}
	}
	levels := credentialLogLevels(t, &out)
	if levels["ERROR"] != 1 || levels["DEBUG"] != 49 || len(levels) != 2 {
		t.Fatalf("50 refused requests logged %v; want one ERROR and 49 DEBUG repeats:\n%s", levels, out.String())
	}

	out.Reset()
	if err := os.WriteFile(password, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(password, later, later); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := selfMetricsAs(t, server, "exporter", "s3cret"); got != http.StatusOK {
			t.Fatalf("with the file back: %d", got)
		}
	}
	if !strings.Contains(out.String(), "can be read again") || strings.Count(out.String(), "can be read again") != 1 {
		t.Fatalf("want one recovery line:\n%s", out.String())
	}
}

// credentialLogLevels counts the lines about the exporter's credential by level.
func credentialLogLevels(t *testing.T, out *bytes.Buffer) map[string]int {
	t.Helper()
	levels := map[string]int{}
	for line := range strings.Lines(out.String()) {
		var record struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%v: %q", err, line)
		}
		if strings.Contains(record.Msg, "Basic Auth credential") {
			levels[record.Level]++
		}
	}
	return levels
}
