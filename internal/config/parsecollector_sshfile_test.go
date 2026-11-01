//go:build !select_request_types || request_type_sshfile

package config

import (
	"strings"
	"testing"
)

// A collector read for the debug page is not asked for the files only a
// request needs, its request block being inert; read WithRequestMade, as
// the playground is with --web.debug.dangerous when it makes its own
// request, it is asked for them as a load asks.
func TestParseCollectorAsksForTheRequestsFilesWhenItIsMade(t *testing.T) {
	doc := []byte("name: logs\nrequest:\n  type: sshfile\n  root: /var/log\n  username: me\n  password_file: /run/secrets/ssh\ntransform:\n  type: prometheus\n")
	cfg, err := ParseCollector(doc, nil)
	if err != nil {
		t.Fatalf("read for a request not made: %v", err)
	}
	if x := cfg.Collectors[0]; !x.RequestNotMade || !x.DescriptorFilesUnread {
		t.Errorf("the request is not marked as never made: %+v", x)
	}
	_, err = ParseCollector(doc, nil, WithRequestMade())
	if err == nil || !strings.Contains(err.Error(), "request.known_hosts_file is required") {
		t.Errorf("read for a request made: %v", err)
	}
}
