//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http2"
)

// readRequestHead reads a request's head off conn.
func readRequestHead(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		if line, err := reader.ReadString('\n'); err != nil || line == "\r\n" {
			return
		}
	}
}

// closedPort is the address of a port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

// A target that resets every connection fails every fetch with an error
// that names the port that connection was made from, another each time:
// the two errors read differently, each in full, and are recognised by one
// text, which names the target and the reset and has the mark for the
// address the connection came from. That holds when the reset comes in
// place of the answer, in the middle of its body, and during the TLS
// handshake. A refused connection, which names only where it went, is
// recognised by its text as it is, and is another failure.
func TestAResetConnectionIsRecognisedWhateverPortItWasMadeFrom(t *testing.T) {
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	instead := resettingTarget(t)
	midBody := connectionTarget(t, func(conn net.Conn) {
		readRequestHead(conn)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nvalue 1\n"))
		// The client has the head and the start of the body before the
		// reset: it is left a second to read them in, since a reset that
		// arrives first takes what was not yet read with it, and the fetch
		// then fails as one reset in place of the answer.
		time.Sleep(time.Second)
		resetConnection(conn)
	})
	handshake := resettingTarget(t)
	for name, tc := range map[string]struct {
		target, same string
	}{
		"in place of the answer":    {"http://" + instead, `HTTP request failed: Get "http://` + instead + `": read tcp #->` + instead + `: read: connection reset by peer`},
		"in the middle of the body": {"http://" + midBody, `reading response: read tcp #->` + midBody + `: read: connection reset by peer`},
		"during the TLS handshake":  {"https://" + handshake, `HTTP request failed: Get "https://` + handshake + `": read tcp #->` + handshake + `: read: connection reset by peer`},
	} {
		c := httpCollector(t, func(c *model.Collector) { c.Request.AllowedSchemes = []string{"http", "https"} })
		a, b := fetchFailures(t, c, tc.target)
		if a.Error() == b.Error() || !strings.Contains(a.Error(), "read tcp 127.0.0.1:") || strings.Contains(a.Error(), model.MovingMark) {
			t.Errorf("%s: the failures read\n%v\n%v\nwant each with the port its connection was made from", name, a, b)
		}
		if model.SameFailureText(a) != tc.same || model.SameFailureText(b) != tc.same {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", name, a, b, model.SameFailureText(a), model.SameFailureText(b), tc.same)
		}
	}
	// A credential in the target's query is withheld from the text the
	// failure is recognised by as it is from the error.
	a, b := fetchFailures(t, httpCollector(t, nil), "http://"+instead+"/status?token=hunter2")
	want := `HTTP request failed: Get "http://` + instead + `/status?token=<redacted>": read tcp #->` + instead + `: read: connection reset by peer`
	if strings.Contains(a.Error()+model.SameFailureText(a), "hunter2") || a.Error() == b.Error() || model.SameFailureText(a) != want || model.SameFailureText(b) != want {
		t.Errorf("with a credential in the query the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), want)
	}
	nobody := closedPort(t)
	a, b = fetchFailures(t, httpCollector(t, nil), "http://"+nobody)
	want = `HTTP request failed: Get "http://` + nobody + `": dial tcp ` + nobody + `: connect: connection refused`
	if a.Error() != want || b.Error() != want || model.SameFailureText(a) != want {
		t.Errorf("a refused connection fails with\n%v\n%v\nrecognised by\n%s\nwant all three\n%s", a, b, model.SameFailureText(a), want)
	}
}

// An HTTP/2 target that ends every request with a stream error fails every
// fetch with an error that names the stream, and the requests of a
// connection are numbered as they are made: the errors of two fetches name
// two streams, and are recognised by one text, when the stream is ended in
// place of the answer and in the middle of its body.
func TestAnHTTP2StreamErrorIsRecognisedWhateverItsStream(t *testing.T) {
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/body" {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("value 1\n"))
			w.(http.Flusher).Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.EnableHTTP2 = true
	})
	for path, same := range map[string]string{
		"/answer": `HTTP request failed: Get "` + server.URL + `/answer": stream error: stream ID #; INTERNAL_ERROR; received from peer`,
		"/body":   `reading response: stream error: stream ID #; INTERNAL_ERROR; received from peer`,
	} {
		a, b := fetchFailures(t, c, server.URL+path)
		if a.Error() == b.Error() || !strings.Contains(a.Error(), "stream error: stream ID ") || strings.Contains(a.Error(), model.MovingMark) {
			t.Errorf("%s: the failures read\n%v\n%v\nwant each with its stream", path, a, b)
		}
		if model.SameFailureText(a) != same || model.SameFailureText(b) != same {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", path, a, b, model.SameFailureText(a), model.SameFailureText(b), same)
		}
	}
}

// An HTTP/2 target that sends a GOAWAY and closes the connection fails the
// fetch with an error that names the last stream the GOAWAY did: two that
// name different streams read differently and are recognised by one text,
// which keeps the error code and what the target said.
func TestAnHTTP2GoAwayIsRecognisedWhateverItsLastStream(t *testing.T) {
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	var connections atomic.Uint32
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.TLS = &tls.Config{NextProtos: []string{http2.NextProtoTLS}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	// The connection is spoken to frame by frame: the request's headers are
	// answered with a GOAWAY that names a stream of its own choosing, at or
	// past the request's, and the end of the connection.
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		http2.NextProtoTLS: func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
			defer func() { _ = conn.Close() }()
			last := 2*connections.Add(1) - 1
			if _, err := io.ReadFull(conn, make([]byte, len(http2.ClientPreface))); err != nil {
				return
			}
			framer := http2.NewFramer(conn, conn)
			if framer.WriteSettings() != nil {
				return
			}
			for {
				frame, err := framer.ReadFrame()
				if err != nil {
					return
				}
				switch frame := frame.(type) {
				case *http2.SettingsFrame:
					if !frame.IsAck() && framer.WriteSettingsAck() != nil {
						return
					}
				case *http2.HeadersFrame:
					_ = framer.WriteGoAway(last, http2.ErrCodeEnhanceYourCalm, []byte("slow down"))
					return
				}
			}
		},
	}
	server.StartTLS()
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.EnableHTTP2 = true
	})
	a, b := fetchFailures(t, c, server.URL)
	said := `: http2: server sent GOAWAY and closed the connection; LastStreamID=%s, ErrCode=ENHANCE_YOUR_CALM, debug="slow down"`
	if !strings.HasSuffix(a.Error(), strings.Replace(said, "%s", "1", 1)) || !strings.HasSuffix(b.Error(), strings.Replace(said, "%s", "3", 1)) {
		t.Fatalf("the failures read\n%v\n%v\nwant a GOAWAY naming stream 1 and one naming stream 3", a, b)
	}
	same := `HTTP request failed: Get "` + server.URL + `"` + strings.Replace(said, "%s", model.MovingMark, 1)
	if model.SameFailureText(a) != same || model.SameFailureText(b) != same {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), same)
	}
}

// A target whose certificate has expired fails every fetch with an error
// that names the time of that attempt beside the time the certificate was
// valid to; it is recognised by its text with the mark for the first, so
// the same failure a second later, or an hour, is the same.
func TestAnExpiredCertificateIsRecognisedWhateverTheTime(t *testing.T) {
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	certificate := expiredCertificate(t)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	a, _ := fetchFailures(t, httpCollector(t, func(c *model.Collector) { c.Request.AllowedSchemes = []string{"https"} }), server.URL)
	said := "tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time "
	validTo := " is after " + leaf.NotAfter.Format(time.RFC3339)
	if !strings.Contains(a.Error(), said+time.Now().Format("2006-01-02T")) || !strings.HasSuffix(a.Error(), validTo) {
		t.Fatalf("the failure reads %v, want it to say %q, the time and %q", a, said, validTo)
	}
	if want := `HTTP request failed: Get "` + server.URL + `": ` + said + model.MovingMark + validTo; model.SameFailureText(a) != want {
		t.Errorf("the failure %v is recognised by\n%s\nwant\n%s", a, model.SameFailureText(a), want)
	}
}

// A compressed answer that turns out corrupt fails the fetch with the
// offset at which it did, which is another in a body of another length:
// the two errors read differently and are recognised by one text.
func TestCorruptCompressedDataIsRecognisedWhateverItsOffset(t *testing.T) {
	var answers atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Blocks of stored data, not closed, and then a block of a type
		// DEFLATE has not got.
		var compressed bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&compressed, gzip.NoCompression)
		_, _ = writer.Write(bytes.Repeat([]byte("value 1\n"), int(answers.Add(1))))
		_ = writer.Flush()
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(append(compressed.Bytes(), 0x07))
	}))
	defer server.Close()
	a, b := fetchFailures(t, httpCollector(t, nil), server.URL)
	if a.Error() == b.Error() || !strings.HasPrefix(a.Error(), "reading response: flate: corrupt input before offset ") || strings.Contains(a.Error(), model.MovingMark) {
		t.Fatalf("the failures read\n%v\n%v\nwant each with its offset", a, b)
	}
	if want := "reading response: flate: corrupt input before offset " + model.MovingMark; model.SameFailureText(a) != want || model.SameFailureText(b) != want {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), want)
	}
}

// A SOCKS proxy that resets every connection fails the fetch with an error
// of two connections, the one through the proxy inside the one to it: the
// port the exporter reached the proxy from differs between two fetches, and
// both are recognised by one text.
func TestAProxysResetIsRecognisedWhateverPortItWasReachedFrom(t *testing.T) {
	proxy := connectionTarget(t, func(conn net.Conn) {
		_, _ = io.ReadFull(conn, make([]byte, 3))
		resetConnection(conn)
	})
	t.Setenv("HTTP_PROXY", "socks5://"+proxy)
	t.Setenv("http_proxy", "socks5://"+proxy)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	a, b := fetchFailures(t, httpCollector(t, nil), "http://target.test:8080")
	if a.Error() == b.Error() || !strings.Contains(a.Error(), "socks connect tcp "+proxy+"->target.test:8080: read tcp 127.0.0.1:") {
		t.Fatalf("the failures read\n%v\n%v\nwant each with the port the proxy was reached from", a, b)
	}
	want := `HTTP request failed: Get "http://target.test:8080": socks connect tcp #->target.test:8080: read tcp #->` + proxy + `: read: connection reset by peer`
	if model.SameFailureText(a) != want || model.SameFailureText(b) != want {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), want)
	}
}
