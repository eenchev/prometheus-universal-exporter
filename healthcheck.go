package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// --healthcheck asks a running exporter whether it is ready, or alive, and
// exits 0 or 1, for a container health check: the image has no curl or wget
// to ask with, and a health check written in Python would start an
// interpreter on every run. It reads nothing but --web.listen-address, starts
// no server and loads no configuration, so a check is one HTTP request.
//
// /health and /ready are never authenticated (AUTHENTICATION.md), so the
// check sends no credentials and needs none of the configuration's.

// healthcheckTimeout bounds the whole check: connecting, sending the request
// and reading the answer. /health and /ready answer at once, from memory, so
// an exporter that takes longer is not healthy; a container runtime or the
// kubelet bounds the command too, and kills it at its own timeout. It is a
// variable for the tests alone, which wait a fraction of it.
var healthcheckTimeout = 5 * time.Second

// healthcheckBodyLimit is how much of a failed check's answer is read for
// its reasons: /ready's are a few short lines.
const healthcheckBodyLimit = 1 << 10

// healthcheckFlag is --healthcheck: off, ready (the flag alone) or health. It
// is a boolean flag that also takes a value, so the common case is the flag
// alone and the liveness check one word more.
type healthcheckFlag struct{ endpoint string }

func (f *healthcheckFlag) String() string {
	if f == nil {
		return ""
	}
	return f.endpoint
}

// IsBoolFlag lets the flag be given alone, which the flag package passes to
// Set as "true".
func (f *healthcheckFlag) IsBoolFlag() bool { return true }

func (f *healthcheckFlag) Set(value string) error {
	switch value {
	case "true", "ready":
		f.endpoint = "ready"
	case "health":
		f.endpoint = "health"
	case "false":
		f.endpoint = ""
	default:
		return fmt.Errorf("--healthcheck %q is not an endpoint; use --healthcheck or --healthcheck=ready for /ready, or --healthcheck=health for /health", value)
	}
	return nil
}

// healthcheckURL is the URL of endpoint on the exporter listening on
// listenAddress, which ValidateListenAddress has accepted. A listener on
// every address is asked on the loopback address of its family: an empty
// host and 0.0.0.0 on 127.0.0.1, which a dual-stack listener answers too,
// and [::] on [::1]. A name or a particular address is asked as it is, since
// the exporter may listen on nothing else.
func healthcheckURL(listenAddress, endpoint string) string {
	host, port, _ := net.SplitHostPort(listenAddress)
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/" + endpoint
}

// runHealthcheck asks the exporter at listenAddress for endpoint and returns
// the exit status: 0 when it answers 200, and 1 otherwise, with one JSON line
// on stderr saying why.
func runHealthcheck(listenAddress, endpoint string, stderr io.Writer) int {
	url := healthcheckURL(listenAddress, endpoint)
	fail := func(args ...any) int {
		newLogger("info", stderr).Error("healthcheck failed", append([]any{"url", url}, args...)...)
		return 1
	}
	client := &http.Client{
		// No proxy: HTTP_PROXY in the container's environment is for its
		// targets, and the exporter is asked where it listens.
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		// Neither endpoint redirects; an answer that does is not theirs.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("error", err.Error())
	}
	response, err := client.Do(request)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return fail("error", fmt.Sprintf("no answer within %s", healthcheckTimeout))
		}
		return fail("error", err.Error())
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		return 0
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, healthcheckBodyLimit))
	// /ready's reasons, one per line, on the one line logged.
	return fail("status", response.StatusCode, "answer", strings.ReplaceAll(strings.TrimSpace(string(body)), "\n", "; "))
}
