package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
// every address (an empty host, 0.0.0.0, [::] or [::ffff:0.0.0.0]) is asked
// on 127.0.0.1: the exporter listens with Go's "tcp" network, whose [::]
// listener is dual-stack and answers IPv4 too, and falls back to IPv4 alone
// on a host without IPv6, where [::1] could not even be dialed. A name or a
// particular address is asked as it is, zone included, since the exporter
// may listen on nothing else. A named port, which the listener resolves, is
// resolved here too, so the URL carries a number.
func healthcheckURL(listenAddress, endpoint string) string {
	host, port, _ := net.SplitHostPort(listenAddress)
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	if _, err := strconv.Atoi(port); err != nil {
		if number, err := net.DefaultResolver.LookupPort(context.Background(), "tcp", port); err == nil {
			port = strconv.Itoa(number)
		}
	}
	// url.URL escapes an IPv6 zone's % as %25, which the URL needs.
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/" + endpoint}).String()
}

// runHealthcheck asks the exporter at listenAddress for endpoint and returns
// the exit status: 0 when it answers 200, and 1 otherwise, with one JSON line
// on stderr saying why.
func runHealthcheck(listenAddress, endpoint string, stderr io.Writer) int {
	target := healthcheckURL(listenAddress, endpoint)
	fail := func(args ...any) int {
		newLogger("info", stderr).Error("healthcheck failed", append([]any{"url", target}, args...)...)
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
