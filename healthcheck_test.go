package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// --healthcheck (healthcheck.go): a container health check without curl.

// healthcheckExporter serves an exporter with conf on 127.0.0.1 and returns
// it and the port it listens on.
func healthcheckExporter(t *testing.T, conf *model.Config) (*exporter.Server, string) {
	t.Helper()
	logger := newLogger("error", io.Discard)
	server := exporter.NewServer(config.NewManager(conf, "", logger), "python3", logger)
	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)
	_, port, err := net.SplitHostPort(listener.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return server, port
}

// A ready exporter passes, the flag alone asking /ready, and the check reads
// no configuration: the --config.file given does not exist.
func TestTheHealthcheckPassesOnAReadyExporter(t *testing.T) {
	_, port := healthcheckExporter(t, &model.Config{})
	for _, args := range [][]string{
		{"--healthcheck"},
		{"--healthcheck=ready"},
		{"--healthcheck=health"},
		{"--healthcheck", "--config.file=/nonexistent/config.yaml", "--static-targets-file=/nonexistent/targets.yaml"},
	} {
		out := runCLI(t, append(args, "--web.listen-address=127.0.0.1:"+port)...)
		if out.code != 0 || out.stdout != "" || out.stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, out.code, out.stdout, out.stderr)
		}
	}
}

// An exporter that is not ready fails the readiness check with one JSON line
// naming the answer and its reason, and still passes the liveness check.
func TestTheHealthcheckFailsAnExporterThatIsNotReady(t *testing.T) {
	server, port := healthcheckExporter(t, &model.Config{})
	server.BeginShutdown()
	address := "--web.listen-address=127.0.0.1:" + port
	out := runCLI(t, "--healthcheck", address)
	if out.code != 1 || out.stdout != "" || len(out.logs) != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q", out.code, out.stdout, out.stderr)
	}
	line := out.logs[0]
	if line["level"] != "ERROR" || line["msg"] != "healthcheck failed" || line["url"] != "http://127.0.0.1:"+port+"/ready" ||
		line["status"] != float64(http.StatusServiceUnavailable) || line["answer"] != "not ready: the exporter is shutting down" {
		t.Errorf("the line logged: %v", line)
	}
	if out := runCLI(t, "--healthcheck=health", address); out.code != 0 {
		t.Errorf("/health of a stopping exporter: exit %d, stderr %q", out.code, out.stderr)
	}
}

// No exporter listening fails the check, saying why.
func TestTheHealthcheckFailsWithoutAnExporter(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	out := runCLI(t, "--healthcheck", "--web.listen-address="+address)
	if out.code != 1 || len(out.logs) != 1 || out.logs[0]["msg"] != "healthcheck failed" || !strings.Contains(out.logs[0]["error"].(string), "refused") {
		t.Fatalf("exit %d, stderr %q", out.code, out.stderr)
	}
}

// An exporter that does not answer within the timeout fails the check.
func TestTheHealthcheckGivesUpOnAnExporterThatDoesNotAnswer(t *testing.T) {
	previous := healthcheckTimeout
	healthcheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { healthcheckTimeout = previous })
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	out := runCLI(t, "--healthcheck", "--web.listen-address="+hung.Listener.Addr().String())
	if out.code != 1 || len(out.logs) != 1 || out.logs[0]["error"] != "no answer within 200ms" {
		t.Fatalf("exit %d, stderr %q", out.code, out.stderr)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the check took %s", elapsed)
	}
}

// /ready needs no credentials with web.basic_auth on, so the check passes
// without the configuration's, which /probe asks for.
func TestTheHealthcheckPassesWithBasicAuthOn(t *testing.T) {
	_, port := healthcheckExporter(t, &model.Config{Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "s3cret"}}})
	response, err := http.Get("http://127.0.0.1:" + port + "/collectors")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/collectors without credentials: %d, so Basic Auth is not on", response.StatusCode)
	}
	for _, check := range []string{"--healthcheck", "--healthcheck=health"} {
		if out := runCLI(t, check, "--web.listen-address=127.0.0.1:"+port); out.code != 0 {
			t.Errorf("%s: exit %d, stderr %q", check, out.code, out.stderr)
		}
	}
}

// A listener on every address is asked on the loopback address of its
// family, a name or a particular address as it is.
func TestTheHealthcheckURLFollowsTheListenAddress(t *testing.T) {
	for address, want := range map[string]string{
		":8080":               "http://127.0.0.1:8080/ready",
		"0.0.0.0:8080":        "http://127.0.0.1:8080/ready",
		"[::]:8080":           "http://[::1]:8080/ready",
		"[::ffff:0.0.0.0]:80": "http://127.0.0.1:80/ready",
		"10.1.2.3:9115":       "http://10.1.2.3:9115/ready",
		"[fd00::7]:9115":      "http://[fd00::7]:9115/ready",
		"exporter.local:9115": "http://exporter.local:9115/ready",
		"localhost:http":      "http://localhost:http/ready",
	} {
		if got := healthcheckURL(address, "ready"); got != want {
			t.Errorf("%s: %s, want %s", address, got, want)
		}
	}
	// End to end, a wildcard listen address reaches the exporter on
	// 127.0.0.1.
	_, port := healthcheckExporter(t, &model.Config{})
	for _, address := range []string{":" + port, "0.0.0.0:" + port} {
		if out := runCLI(t, "--healthcheck", "--web.listen-address="+address); out.code != 0 {
			t.Errorf("%s: exit %d, stderr %q", address, out.code, out.stderr)
		}
	}
}

// Only ready and health are endpoints to check; anything else, and an
// endpoint given after a space, is a command-line error.
func TestAHealthcheckOfAnotherEndpointIsACommandLineError(t *testing.T) {
	for args, want := range map[string]string{
		"--healthcheck=live":   `--healthcheck \"live\" is not an endpoint; use --healthcheck or --healthcheck=ready for /ready, or --healthcheck=health for /health`,
		"--healthcheck=/ready": `--healthcheck \"/ready\" is not an endpoint`,
		"--healthcheck health": `unexpected argument \"health\"`,
	} {
		if out := runCLI(t, strings.Fields(args)...); out.code != 2 || !strings.Contains(out.stderr, want) {
			t.Errorf("%s: exit %d, stderr %q", args, out.code, out.stderr)
		}
	}
}
