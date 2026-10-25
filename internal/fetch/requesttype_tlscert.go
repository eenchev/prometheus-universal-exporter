//go:build !select_request_types || request_type_tlscert

package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The tlscert request type makes a TLS handshake with the target and hands
// the certificates the server presented to the transforms as a JSON
// document: for each, its subject, names, issuer, serial and validity, and
// for the chain whether it verifies.
//
//	request:
//	  type: tlscert
//	  tls:
//	    ca_file: /etc/exporter/ca.pem   # verify against these besides the system's roots
//	    server_name: www.example.com    # SNI, and the name verified
//
// The probe's target is host:port, port 443 when it is left out. The
// handshake is the scrape: nothing is sent over the connection, which is
// closed when the handshake is over.
//
// A chain that does not verify does not fail the scrape: the expiry of a
// certificate nobody trusts is still worth knowing, and why it is not
// trusted is in the document, verified false and the error's text beside
// it. A handshake that fails, a connection refused, or a deadline that
// comes first does fail it.
//
// The tls block is the one http, grpc and sql take, read by the same code
// (tlsConfig): ca_file, a client certificate, and server_name. Its
// insecure_skip_verify is refused, since no certificate is refused anyway.
//
// This file imports nothing outside the standard library.

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeTLSCert,
		Fields:       []string{"tls", "allowed_targets", "denied_targets"},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Validate:     validateTLSCertRequest,
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) string { return "host or host:port" },
		Fetch:        fetchTLSCert,
		SchemaRule:   tlsCertSchemaRule,
		CheckTarget: func(_ *model.Collector, target string, _ bool) error {
			_, err := parseTLSCertTarget(target)
			return err
		},
		Label: func(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
			address, err := parseTLSCertTarget(target)
			if err != nil {
				return "", err
			}
			return "tls://" + address.hostPort, nil
		},
		Method:     func(*model.Collector, RequestOverrides) string { return tlsCertMethod },
		Display:    tlsCertDisplay,
		Stage:      "tls",
		CheckFiles: checkTLSCertFiles,
	})
}

// tlsCertMethod is the http_method label of a handshake.
const tlsCertMethod = "HANDSHAKE"

// tlsCertDefaultPort is the port of a target that names none.
const tlsCertDefaultPort = "443"

// tlsCertTimeout bounds a handshake with no deadline of its own.
const tlsCertTimeout = 30 * time.Second

func validateTLSCertRequest(x *model.Collector) error {
	t := x.Request.TLS
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if t.InsecureSkipVerify {
		return fmt.Errorf("collector %q sets request.tls.insecure_skip_verify, which a tlscert collector has no use for: a chain that does not verify is reported, verified false with the reason, and never fails the scrape", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	return nil
}

// tlsCertAddress is a target as a handshake reaches it: host as the policy
// matches it and the name is verified, port and hostPort what is dialed.
type tlsCertAddress struct {
	host, hostPort string
	port           int
}

// parseTLSCertTarget reads a target: host, host:port or [v6]:port, port 443
// when it names none. Nothing else may come with it.
func parseTLSCertTarget(raw string) (tlsCertAddress, error) {
	target := strings.TrimSpace(raw)
	if strings.Contains(target, "://") {
		return tlsCertAddress{}, fmt.Errorf("target %q has a scheme; a tlscert target is host or host:port, such as example.com:443", raw)
	}
	if strings.ContainsAny(target, "/?#@") {
		return tlsCertAddress{}, fmt.Errorf("target %q has a path, query or user in it; a tlscert target is only the host and its port", raw)
	}
	host, port := target, tlsCertDefaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return tlsCertAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:443", raw)
	}
	if host == "" {
		return tlsCertAddress{}, fmt.Errorf("target %q names no host; a tlscert target is host or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return tlsCertAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return tlsCertAddress{}, err
	}
	return tlsCertAddress{host: canonical, port: n, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// tlsCertDisplay renders a target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func tlsCertDisplay(target string) string {
	if _, err := parseTLSCertTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// tlsCertDocument is a handshake as the transforms read it.
type tlsCertDocument struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ServerName string `json:"server_name"`
	TLSVersion string `json:"tls_version"`
	Cipher     string `json:"cipher_suite"`
	Verified   bool   `json:"verified"`
	// VerifyError is why the chain does not verify, "" when it does.
	VerifyError     string  `json:"verify_error"`
	DurationSeconds float64 `json:"duration_seconds"`
	// NotAfter and SecondsUntilExpiry are the first certificate's, the
	// server's own, repeated here for the rule that wants nothing else.
	NotAfter           int64             `json:"not_after"`
	SecondsUntilExpiry int64             `json:"seconds_until_expiry"`
	Certificates       []tlsCertificate  `json:"certificates"`
	Chain              *tlsCertChainInfo `json:"chain"`
}

// tlsCertChainInfo is what is true of the chain as a whole.
type tlsCertChainInfo struct {
	// Length is the number of certificates presented.
	Length int `json:"length"`
	// EarliestNotAfter and SecondsUntilEarliestExpiry are when the chain
	// stops verifying: of a chain that verifies, the latest of the chains
	// it verifies by, each ending when the first of its certificates, its
	// root included, expires, since a client trusts the server while any
	// of them holds; of one that does not, the first of the presented
	// certificates to expire. A certificate the server still sends but no
	// client needs, such as an expired cross-signed root, would otherwise
	// hold the expiry in the past for good.
	EarliestNotAfter           int64 `json:"earliest_not_after"`
	SecondsUntilEarliestExpiry int64 `json:"seconds_until_earliest_expiry"`
	// EarliestFrom is which certificates EarliestNotAfter is of:
	// "verified", the chains the chain verifies by, or "presented".
	EarliestFrom string `json:"earliest_from"`
}

// The values of tlsCertChainInfo.EarliestFrom.
const (
	tlsCertEarliestFromVerified  = "verified"
	tlsCertEarliestFromPresented = "presented"
)

// tlsCertificate is one certificate of the chain.
type tlsCertificate struct {
	SubjectCN          string   `json:"subject_cn"`
	Subject            string   `json:"subject"`
	IssuerCN           string   `json:"issuer_cn"`
	Issuer             string   `json:"issuer"`
	Serial             string   `json:"serial"`
	DNSNames           []string `json:"dns_names"`
	IPAddresses        []string `json:"ip_addresses"`
	EmailAddresses     []string `json:"email_addresses"`
	URIs               []string `json:"uris"`
	NotBefore          int64    `json:"not_before"`
	NotBeforeRFC3339   string   `json:"not_before_rfc3339"`
	NotAfter           int64    `json:"not_after"`
	NotAfterRFC3339    string   `json:"not_after_rfc3339"`
	SecondsUntilExpiry int64    `json:"seconds_until_expiry"`
	IsCA               bool     `json:"is_ca"`
	SignatureAlgorithm string   `json:"signature_algorithm"`
	SHA256Fingerprint  string   `json:"sha256_fingerprint"`
}

// tlsCertNow is the time a certificate's validity is measured from; a test
// sets it.
var tlsCertNow = time.Now

// fetchTLSCert makes a scrape's handshake and answers the chain as JSON.
func fetchTLSCert(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseTLSCertTarget(target)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tlsCertTimeout)
		defer cancel()
	}
	cfg, err := tlsConfig(c.Request.TLS)
	if err != nil {
		return nil, fmt.Errorf("request.tls: %w", err)
	}
	if cfg.ServerName == "" {
		cfg.ServerName = address.host
	}
	// The chain is verified below, after the handshake, so that one that
	// does not verify is reported rather than refused; the handshake itself
	// takes whatever the server presents.
	roots, serverName := cfg.RootCAs, cfg.ServerName
	cfg.InsecureSkipVerify = true //nolint:gosec // G402: verified below, and the outcome reported, not acted on
	// The host's name now; the address the connection is made to when it
	// is made, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "tls", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	clientCertificate := "no"
	if c.Request.TLS.CertFile != "" {
		clientCertificate = "yes"
	}
	traceRequest(ctx, tlsCertMethod, "tls://"+address.hostPort, http.Header{"Tls-Server-Name": {serverName}, "Tls-Client-Certificate": {clientCertificate}}, "", false)
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", address.hostPort, err)
		}
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	tlsConn := tls.Client(conn, cfg)
	defer func() { _ = tlsConn.Close() }()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		err = fmt.Errorf("TLS handshake with %s: %w", address.hostPort, err)
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	state := tlsConn.ConnectionState()
	now := tlsCertNow()
	doc := tlsCertDocument{
		Host: address.host, Port: address.port, ServerName: serverName,
		TLSVersion: tls.VersionName(state.Version), Cipher: tls.CipherSuiteName(state.CipherSuite),
		Certificates: make([]tlsCertificate, 0, len(state.PeerCertificates)),
	}
	verifiedChains, err := verifyTLSCertChain(state.PeerCertificates, roots, serverName, now)
	if err != nil {
		doc.VerifyError = err.Error()
	} else {
		doc.Verified = true
	}
	for _, cert := range state.PeerCertificates {
		doc.Certificates = append(doc.Certificates, describeCertificate(cert, now))
	}
	if len(doc.Certificates) > 0 {
		doc.NotAfter, doc.SecondsUntilExpiry = doc.Certificates[0].NotAfter, doc.Certificates[0].SecondsUntilExpiry
		chain := &tlsCertChainInfo{Length: len(doc.Certificates)}
		if len(verifiedChains) > 0 {
			chain.EarliestNotAfter, chain.EarliestFrom = verifiedChainsExpiry(verifiedChains).Unix(), tlsCertEarliestFromVerified
		} else {
			chain.EarliestNotAfter, chain.EarliestFrom = firstExpiry(state.PeerCertificates).Unix(), tlsCertEarliestFromPresented
		}
		chain.SecondsUntilEarliestExpiry = chain.EarliestNotAfter - now.Unix()
		doc.Chain = chain
	}
	doc.DurationSeconds = time.Since(start).Seconds()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	presented := fmt.Sprintf("%d certificates", len(doc.Certificates))
	if len(doc.Certificates) == 1 {
		presented = "1 certificate"
	}
	outcome := doc.TLSVersion + ", " + presented + ", verified"
	if !doc.Verified {
		outcome = doc.TLSVersion + ", " + presented + ", not verified: " + doc.VerifyError
	}
	traceOutcome(ctx, outcome)
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// verifyTLSCertChain verifies the chain the server presented, as a TLS
// client would: the first certificate against serverName, the others as
// intermediates, up to roots, the system's when nil, at now. It returns
// the chains it verifies by, each from the server's certificate to a root.
func verifyTLSCertChain(certs []*x509.Certificate, roots *x509.CertPool, serverName string, now time.Time) ([][]*x509.Certificate, error) {
	if len(certs) == 0 {
		return nil, errors.New("the server presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	return certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: serverName, CurrentTime: now})
}

// verifiedChainsExpiry is when the last of chains stops verifying, each
// at the expiry of the first of its certificates, its root included: a
// client trusts the server while any one of them holds.
func verifiedChainsExpiry(chains [][]*x509.Certificate) time.Time {
	var latest time.Time
	for _, chain := range chains {
		if end := firstExpiry(chain); end.After(latest) {
			latest = end
		}
	}
	return latest
}

// firstExpiry is the not_after of the first of certs to expire.
func firstExpiry(certs []*x509.Certificate) time.Time {
	first := certs[0].NotAfter
	for _, cert := range certs[1:] {
		if cert.NotAfter.Before(first) {
			first = cert.NotAfter
		}
	}
	return first
}

// describeCertificate is cert as the document shows it, its expiry
// measured from now.
func describeCertificate(cert *x509.Certificate, now time.Time) tlsCertificate {
	sum := sha256.Sum256(cert.Raw)
	d := tlsCertificate{
		SubjectCN: cert.Subject.CommonName, Subject: cert.Subject.String(),
		IssuerCN: cert.Issuer.CommonName, Issuer: cert.Issuer.String(),
		Serial:         strings.ToLower(cert.SerialNumber.Text(16)),
		DNSNames:       append([]string{}, cert.DNSNames...),
		IPAddresses:    make([]string, 0, len(cert.IPAddresses)),
		EmailAddresses: append([]string{}, cert.EmailAddresses...),
		URIs:           make([]string, 0, len(cert.URIs)),
		NotBefore:      cert.NotBefore.Unix(), NotBeforeRFC3339: cert.NotBefore.UTC().Format(time.RFC3339),
		NotAfter: cert.NotAfter.Unix(), NotAfterRFC3339: cert.NotAfter.UTC().Format(time.RFC3339),
		SecondsUntilExpiry: cert.NotAfter.Unix() - now.Unix(),
		IsCA:               cert.IsCA,
		SignatureAlgorithm: cert.SignatureAlgorithm.String(),
		SHA256Fingerprint:  hex.EncodeToString(sum[:]),
	}
	for _, ip := range cert.IPAddresses {
		d.IPAddresses = append(d.IPAddresses, ip.String())
	}
	for _, u := range cert.URIs {
		d.URIs = append(d.URIs, u.String())
	}
	return d
}

// checkTLSCertFiles reads the tls block's files as a handshake reads them,
// for --dry-run: a CA file missing or holding no certificate, a client
// certificate or key that does not load. Loading the configuration reads
// none of them, since a file may be mounted after the exporter starts.
func checkTLSCertFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil {
		return nil
	}
	if _, err := tlsConfig(c.Request.TLS); err != nil {
		return []string{fmt.Sprintf("collector %q: request.tls: %v; a scrape would fail on it now (the files are read at each handshake, so one put in place later is used then)", c.Name, err)}
	}
	return nil
}

// tlsCertSchemaRule is what a tlscert collector's request is held to beyond its
// keys, as the exporter holds it (validateTLSCertRequest): its tls
// block takes no insecure_skip_verify true, since a chain that does not
// verify is reported and never refused.
func tlsCertSchemaRule() map[string]any {
	return map[string]any{"properties": map[string]any{"tls": map[string]any{"properties": map[string]any{"insecure_skip_verify": map[string]any{"not": map[string]any{"const": true}}}}}}
}
