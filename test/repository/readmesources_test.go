package repository

import (
	"path/filepath"
	"strings"
	"testing"
)

// The README's opening is where a reader decides whether this reads their
// source at all. It started as an HTTP exporter and kept saying so after it
// learned gRPC, Graphite, files, SSH, SQL, S3, Kubernetes, Redis, TCP and
// LDAP; this keeps the opening's table of sources naming every request type
// in the tree, and makes a new request type say how the README should name
// it.

// readmeSourceNames is, per request type, the words that the README's table
// of sources uses for it.
var readmeSourceNames = map[string]string{
	"http":       "HTTP APIs",
	"grpc":       "gRPC services",
	"graphite":   "Graphite servers",
	"localfile":  "Local files",
	"sshfile":    "files on another host over SSH",
	"sql":        "SQL databases",
	"dns":        "DNS records",
	"tlscert":    "TLS certificates",
	"s3file":     "objects in Amazon S3",
	"kubernetes": "Kubernetes objects",
	"redis":      "Redis and Valkey",
	"tcp":        "Services that answer a line over TCP",
	"ldap":       "LDAP directories",
	"sshexec":    "Commands run on another host over SSH",
	"ntp":        "NTP clocks",
	"icmp":       "ping (ICMP)",
	"snmp":       "SNMP agents",
	"smtp":       "Mail servers (SMTP)",
	"modbus":     "Modbus TCP devices",
	"mqtt":       "MQTT topics",
	"websocket":  "WebSocket endpoints",
	"ftp":        "Files on an FTP server",
	"imap":       "IMAP",
	"pop3":       "POP3",
	"radius":     "RADIUS logins",
	"whois":      "domain registrations (WHOIS)",
	"sip":        "SIP servers",
	"amqp":       "AMQP queues",
	"nats":       "NATS with JetStream",
	"kafka":      "Kafka consumer lag",
}

// readmeOpening returns the README's opening: what stands between the badges
// and the first section, the tagline and the table of sources among it.
func readmeOpening(t *testing.T) string {
	t.Helper()
	intro, _, ok := strings.Cut(read(t, "README.md"), "\n## Try it")
	if !ok {
		t.Fatal("README.md has no \"## Try it\" section; the opening has moved")
	}
	if !strings.Contains(intro, "| Default build | Opt-in build |") {
		t.Fatal("README.md's opening has no table of sources by build; the opening has moved")
	}
	return strings.Join(strings.Fields(intro), " ")
}

func TestReadmeOpeningNamesEveryRequestType(t *testing.T) {
	files, err := filepath.Glob("internal/fetch/requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "requesttype_"), ".go")
		if name == "none" {
			continue
		}
		types = append(types, name)
	}
	if len(types) < 6 {
		t.Fatalf("found %d request types under internal/fetch; the files have moved", len(types))
	}
	opening := readmeOpening(t)
	for _, typ := range types {
		name, ok := readmeSourceNames[typ]
		if !ok {
			t.Errorf("request type %q has no entry in readmeSourceNames: name it in the README's table of sources, then add it", typ)
			continue
		}
		if !strings.Contains(opening, name) {
			t.Errorf("the README's table of sources does not name %q (%s)", name, typ)
		}
	}
}
