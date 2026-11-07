//go:build !select_request_types || request_type_tlscert

package fetch

import (
	"bufio"
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
	"golang.org/x/crypto/ocsp"
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
// A server that starts in plaintext and turns to TLS when asked, a mail or
// FTP server, is read with starttls: smtp, imap, pop3 or ftp. The greeting
// is read and the protocol's request for TLS spoken (startTLS, which tcp
// speaks too), and the handshake is made once the server has said yes;
// the target's port is then the protocol's own when it is left out, 25,
// 143, 110 or 21 (tlsCertStartTLSPorts). What the server said before TLS is
// in the debug report and in an error, quoted and withheld from a probe's
// caller, and never in the document, which says only which protocol began
// TLS.
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
		Fields:       []string{"tls", "starttls", "allowed_targets", "denied_targets"},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    tlsCertRequester{},
		JSONAnswer:   true,
		TargetHint:   tlsCertTargetHint,
		AnswerHint:   "Paste the handshake as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		SchemaRule: tlsCertSchemaRule,
		Stage:      "tls",
		// start_tls is ldap's StartTLS, a boolean: this type's names the
		// protocol that asks for TLS.
		KeyAdvice: map[string]string{"start_tls": "a tlscert collector begins TLS on its plaintext connection with starttls, which names the protocol that asks for it: smtp, imap, pop3 or ftp"},

		CheckFiles: checkTLSCertFiles,
	})
}

// tlsCertRequester is the tlscert request type's Requester.
type tlsCertRequester struct{}

// Validate checks a tlscert collector's request and fills in its defaults.
func (tlsCertRequester) Validate(c *model.Collector) error {
	return validateTLSCertRequest(c)
}

// Fetch performs one request of a tlscert collector.
func (tlsCertRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchTLSCert(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a tlscert collector's target.
func (tlsCertRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, err := parseTLSCertTarget(target, c.Request.StartTLS)
	return err
}

// Label is the url label of a tlscert request.
func (tlsCertRequester) Label(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseTLSCertTarget(target, c.Request.StartTLS)
	if err != nil {
		return "", err
	}
	return "tls://" + address.hostPort, nil
}

// Method is the http_method label of a tlscert request.
func (tlsCertRequester) Method(*model.Collector, RequestOverrides) string { return tlsCertMethod }

// Display renders a tlscert target for logs and labels.
func (tlsCertRequester) Display(target string) string {
	return tlsCertDisplay(target)
}

// tlsCertTargetHint is what the collectors page and the debug page say of a
// target: host or host:port, the port the one a target that names none is
// reached on, 443 or request.starttls's protocol's.
func tlsCertTargetHint(c *model.Collector) (string, string) {
	port := tlsCertDefaultPort
	if p, ok := tlsCertStartTLSPorts[c.Request.StartTLS]; ok {
		port = p
	}
	return "Server", "host or host:" + port
}

// tlsCertMethod is the http_method label of a handshake.
const tlsCertMethod = "HANDSHAKE"

// tlsCertDefaultPort is the port of a target that names none.
const tlsCertDefaultPort = "443"

// tlsCertStartTLSPorts are the ports of a target that names none with
// request.starttls: each the port registered under the protocol's own name,
// where its servers begin TLS with STARTTLS. SMTP's is 25, where a domain's
// mail is delivered, rather than 587, where a user's client submits it: a
// mail server's certificate is the one others deliver to it with; the
// submission port is written as :587.
var tlsCertStartTLSPorts = map[string]string{startTLSSMTP: "25", startTLSIMAP: "143", startTLSPOP3: "110", startTLSFTP: "21"}

// tlsCertLimitKeys is the key that sets the bound of what a server says
// before TLS: tlscert reads no body, so takes no request.max_response_bytes.
const tlsCertLimitKeys = "limits.max_response_bytes"

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
	return validateStartTLS(x.Name, RequestTypeTLSCert, x.Request.StartTLS)
}

// tlsCertAddress is a target as a handshake reaches it: host as the policy
// matches it and the name is verified, port and hostPort what is dialed.
type tlsCertAddress struct {
	host, hostPort string
	port           int
}

// parseTLSCertTarget reads a target: host, host:port or [v6]:port, port 443
// when it names none, or, with starttls, the protocol's own port. Nothing
// else may come with it.
func parseTLSCertTarget(raw, starttls string) (tlsCertAddress, error) {
	target := strings.TrimSpace(raw)
	if strings.Contains(target, "://") {
		return tlsCertAddress{}, fmt.Errorf("target %q has a scheme; a tlscert target is host or host:port, such as example.com:443", raw)
	}
	if strings.ContainsAny(target, "/?#@") {
		return tlsCertAddress{}, fmt.Errorf("target %q has a path, query or user in it; a tlscert target is only the host and its port", raw)
	}
	host, port := target, tlsCertDefaultPort
	if p, ok := tlsCertStartTLSPorts[starttls]; ok {
		port = p
	}
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
	if _, err := parseTLSCertTarget(target, ""); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// tlsCertDocument is a handshake as the transforms read it.
type tlsCertDocument struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ServerName string `json:"server_name"`
	// StartTLS is the protocol whose request began TLS, "" for a server
	// that spoke TLS from the start.
	StartTLS   string `json:"starttls"`
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
	// OCSP is the OCSP response the server stapled to the handshake, if
	// any (tlsCertOCSP).
	OCSP tlsCertOCSP `json:"ocsp"`
}

// tlsCertOCSP is the OCSP response the server stapled to the handshake:
// whether there was one, the status it gives the server's certificate,
// good, revoked or unknown, whether it is signed by the certificate's
// issuer, or a responder that issuer delegated to, for that certificate,
// why not or why it could not be read, and its times, Unix seconds, 0 for
// one it does not have.
type tlsCertOCSP struct {
	Stapled  bool   `json:"stapled"`
	Status   string `json:"status"`
	Verified bool   `json:"verified"`
	Error    string `json:"error"`
	// ProducedAt is when the responder signed it; ThisUpdate when the
	// status was known to be true; NextUpdate when a newer one is due, 0
	// when the responder names no time, and SecondsUntilNextUpdate that
	// less the time of the scrape, negative for a staple gone stale.
	ProducedAt             int64 `json:"produced_at"`
	ThisUpdate             int64 `json:"this_update"`
	NextUpdate             int64 `json:"next_update"`
	SecondsUntilNextUpdate int64 `json:"seconds_until_next_update"`
	// RevokedAt and RevocationReason are of a revoked certificate.
	RevokedAt        int64  `json:"revoked_at"`
	RevocationReason string `json:"revocation_reason"`
}

// ocspStatuses are the names of the statuses an OCSP response gives.
var ocspStatuses = map[int]string{ocsp.Good: "good", ocsp.Revoked: "revoked", ocsp.Unknown: "unknown"}

// ocspReasons are the names RFC 5280 gives the reasons of a revocation.
var ocspReasons = map[int]string{
	ocsp.Unspecified: "unspecified", ocsp.KeyCompromise: "keyCompromise", ocsp.CACompromise: "cACompromise",
	ocsp.AffiliationChanged: "affiliationChanged", ocsp.Superseded: "superseded", ocsp.CessationOfOperation: "cessationOfOperation",
	ocsp.CertificateHold: "certificateHold", ocsp.RemoveFromCRL: "removeFromCRL", ocsp.PrivilegeWithdrawn: "privilegeWithdrawn",
	ocsp.AACompromise: "aACompromise",
}

// stapledOCSP reads the OCSP response raw the server stapled for leaf,
// checked against issuer, the certificate after it in the chain it
// verified by, or as presented: its signature and that it is of leaf. A
// response that cannot be read says why, with no status; one whose
// signature does not check is read for its status and times all the same,
// with verified false and why.
func stapledOCSP(raw []byte, leaf, issuer *x509.Certificate, now time.Time) tlsCertOCSP {
	out := tlsCertOCSP{Stapled: len(raw) > 0}
	if !out.Stapled || leaf == nil {
		return out
	}
	var response *ocsp.Response
	var err error
	if issuer == nil {
		err = errors.New("no issuer certificate was presented to check its signature against")
	} else if response, err = ocsp.ParseResponseForCert(raw, leaf, issuer); err == nil {
		out.Verified = true
	}
	if response == nil {
		// Read without the issuer, for what it says, when it is no
		// response of the certificate's issuer for it.
		read, readErr := ocsp.ParseResponse(raw, nil)
		if readErr != nil {
			out.Error = "the stapled response cannot be read: " + readErr.Error()
			return out
		}
		response = read
		if issuer != nil && response.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
			err = errors.New("it is of another certificate than the server's")
		}
	}
	if err != nil {
		out.Error = "the stapled response is not verified: " + err.Error()
	}
	out.Status = ocspStatuses[response.Status]
	out.ProducedAt, out.ThisUpdate = response.ProducedAt.Unix(), response.ThisUpdate.Unix()
	if !response.NextUpdate.IsZero() {
		out.NextUpdate = response.NextUpdate.Unix()
		out.SecondsUntilNextUpdate = out.NextUpdate - now.Unix()
	}
	if response.Status == ocsp.Revoked {
		out.RevokedAt, out.RevocationReason = response.RevokedAt.Unix(), ocspReasons[response.RevocationReason]
	}
	return out
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
	address, err := parseTLSCertTarget(target, c.Request.StartTLS)
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
	traced := http.Header{"Tls-Server-Name": {serverName}, "Tls-Client-Certificate": {clientCertificate}}
	if protocol := c.Request.StartTLS; protocol != "" {
		traced.Set("Tls-Starttls", protocol)
	}
	traceRequest(ctx, tlsCertMethod, "tls://"+address.hostPort, traced, "", false)
	// The lookup, the connection and the handshake are the request's
	// phases, whether the handshake went through or not.
	timedCtx, timing := withConnTiming(ctx)
	defer func() { tracePhases(ctx, timing.phases()) }()
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(timedCtx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", address.hostPort, err)
		}
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	began := ""
	if c.Request.StartTLS != "" {
		if began, err = tlsCertStartTLS(ctx, conn, address, c); err != nil {
			_ = conn.Close()
			traceFailedOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
	}
	tlsConn := tls.Client(conn, cfg)
	defer func() { _ = tlsConn.Close() }()
	if err := timing.handshake("tls", func() error { return tlsConn.HandshakeContext(ctx) }); err != nil {
		err = fmt.Errorf("TLS handshake with %s: %w", address.hostPort, err)
		// After STARTTLS the report tells the dialogue that led to the
		// handshake, as it does before a handshake that succeeds.
		traceFailedOutcome(ctx, began+"error: "+err.Error())
		return nil, err
	}
	state := tlsConn.ConnectionState()
	now := tlsCertNow()
	doc := tlsCertDocument{
		Host: address.host, Port: address.port, ServerName: serverName, StartTLS: c.Request.StartTLS,
		TLSVersion: tls.VersionName(state.Version), Cipher: tls.CipherSuiteName(state.CipherSuite),
		Certificates: make([]tlsCertificate, 0, len(state.PeerCertificates)),
	}
	verifiedChains, err := verifyTLSCertChain(state.PeerCertificates, roots, serverName, now)
	if err != nil {
		doc.VerifyError = err.Error()
	} else {
		doc.Verified = true
	}
	if len(state.PeerCertificates) > 0 {
		var issuer *x509.Certificate
		switch {
		case len(verifiedChains) > 0 && len(verifiedChains[0]) > 1:
			issuer = verifiedChains[0][1]
		case len(state.PeerCertificates) > 1:
			issuer = state.PeerCertificates[1]
		}
		doc.OCSP = stapledOCSP(state.OCSPResponse, state.PeerCertificates[0], issuer, now)
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
	traceOutcome(ctx, began+outcome)
	traceFacts(ctx, tlsCertFacts(&doc, state)...)
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

// tlsCertStartTLS asks the server conn reaches to begin TLS as
// request.starttls's protocol does (startTLS): it reads the greeting, which
// must be the protocol's ready one, and speaks the protocol's request for
// TLS, returning how the debug report tells it, the greeting and each
// command and answer quoted, for the handshake's outcome to follow. The
// reads and writes end at the scrape's deadline, or when it is cancelled;
// the handshake, which the caller makes next, is bounded by the context
// alone, as one without STARTTLS is.
func tlsCertStartTLS(ctx context.Context, conn net.Conn, address tlsCertAddress, c *model.Collector) (string, error) {
	protocol := c.Request.StartTLS
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	// The reader is dropped before the handshake: startTLS fails a server
	// that wrote anything after its yes, so it holds nothing the handshake
	// would miss.
	reader := bufio.NewReader(conn)
	limit := responseLimit(c)
	before, greeting, err := readStartTLSGreeting(ctx, reader, protocol, address.hostPort, limit, tlsCertLimitKeys, "request.starttls")
	if err != nil {
		return "", err
	}
	if err := startTLSGreeted(protocol, address.hostPort, greeting); err != nil {
		return "", err
	}
	steps, err := startTLS(ctx, conn, reader, protocol, address.hostPort, conn.LocalAddr(), limit, tlsCertLimitKeys)
	if err != nil {
		return "", err
	}
	stop()
	_ = conn.SetDeadline(time.Time{})
	return "greeted with " + tcpShown(string(before)+string(greeting)) + "; " + startTLSShown(steps) + "; ", nil
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

// tlsCertFacts are what a handshake's span tells (TracedRequest.Facts): by
// OpenTelemetry's conventions the protocol's version and the cipher, by
// the names Go and the IANA registry give them, and when the first
// certificate expires; and how many certificates the server presented,
// whether the chain verified, whether an OCSP response was stapled and the
// status it gives, and the days the first has left, a fraction of a day
// included. Never a certificate's subject or issuer, which the
// server wrote, nor why the chain did not verify.
func tlsCertFacts(doc *tlsCertDocument, state tls.ConnectionState) []TraceFact {
	facts := []TraceFact{
		nameFact("tls.protocol.name", "tls"),
		nameFact("tls.protocol.version", strings.TrimPrefix(doc.TLSVersion, "TLS ")),
		nameFact("tls.cipher", tls.CipherSuiteName(state.CipherSuite)),
		intFact("tlscert.certificates", len(doc.Certificates)),
		boolFact("tlscert.verified", doc.Verified),
		boolFact("tlscert.ocsp.stapled", doc.OCSP.Stapled),
	}
	if doc.OCSP.Status != "" {
		facts = append(facts, nameFact("tlscert.ocsp.status", doc.OCSP.Status))
	}
	if len(doc.Certificates) > 0 {
		facts = append(facts,
			nameFact("tls.server.not_after", time.Unix(doc.NotAfter, 0).UTC().Format(time.RFC3339)),
			doubleFact("tlscert.leaf.days_until_expiry", float64(doc.SecondsUntilExpiry)/86400),
		)
	}
	return facts
}
