//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// bannerServer answers every connection with banner and closes it: a
// server of another protocol, which says who it is before it is asked.
func bannerServer(t *testing.T, banner string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(banner))
			_ = conn.Close()
		}
	}()
	return "http://" + listener.Addr().String()
}

// What Go's HTTP client quotes of an answer that is no HTTP — the first line
// of an SSH or SMTP banner, a Redis error — is the operator's: the error
// has it, and a probe's caller is told the error up to Go's words and
// model.WithheldMark after them, for a target's request and for a login's.
func TestWhatGoQuotesOfAnAnswerThatIsNoHTTPIsTheOperators(t *testing.T) {
	freshLogins(t)
	banner := bannerServer(t, "SSH-2.0-OpenSSH_9.6 secret-host\r\n")
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))
	t.Cleanup(data.Close)
	plain := testutil.Collector("c", "json")
	for name, fetch := range map[string]func(ctx context.Context) error{
		"the target": func(ctx context.Context) error {
			_, err := FetchCollector(ctx, banner, &plain, RequestOverrides{}, nil)
			return err
		},
		"a login": func(ctx context.Context) error {
			c := loginCollector(t, model.LoginConfig{Type: "json", URL: banner + "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
			_, err := FetchCollector(ctx, data.URL, c, RequestOverrides{}, nil)
			return err
		},
		"an OAuth 2.0 login": func(ctx context.Context) error {
			c := loginCollector(t, model.LoginConfig{Type: "oauth2", URL: banner + "/token", ClientID: "exporter", ClientSecretFile: secretFiles(t, map[string]string{"secret": "s"})["secret"]})
			_, err := FetchCollector(ctx, data.URL, c, RequestOverrides{}, nil)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := fetch(ctx)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "secret-host") {
			t.Fatalf("%s: got %v, want what the server said in the error", name, err)
		}
		if told := model.CallerText(err); strings.Contains(told, "secret-host") || !strings.Contains(told, ": malformed HTTP ") || !strings.HasSuffix(told, " "+model.WithheldMark) {
			t.Errorf("%s: the caller is told %q", name, told)
		}
	}
}
