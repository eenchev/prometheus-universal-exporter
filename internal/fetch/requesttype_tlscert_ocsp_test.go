//go:build !select_request_types || request_type_tlscert

package fetch

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/crypto/ocsp"
)

// ocspResponse is an OCSP response of leaf, of status, signed by signer for
// the issuer issuer.
func ocspResponse(t *testing.T, issuer, signer, leaf *testCert, status int, now time.Time) []byte {
	t.Helper()
	template := ocsp.Response{
		Status: status, SerialNumber: leaf.cert.SerialNumber,
		ThisUpdate: now.Add(-time.Hour), NextUpdate: now.Add(48 * time.Hour),
	}
	if status == ocsp.Revoked {
		template.RevokedAt, template.RevocationReason = now.Add(-2*time.Hour), ocsp.KeyCompromise
	}
	raw, err := ocsp.CreateResponse(issuer.cert, issuer.cert, template, signer.key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A server that staples an OCSP response has it in the document's ocsp:
// stapled, the status it gives the server's certificate, its times and a
// revocation's time and reason, verified when the certificate's issuer
// signed it for that certificate; signed by another, or of another
// certificate, it is read with verified false and why; bytes that are no
// response say so with no status; and a server that staples none has
// stapled false. The span tells whether one was stapled and its status.
func TestATLSCertCollectorReadsTheStapledOCSPResponse(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "localhost", dns: []string{"localhost"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(30 * 24 * time.Hour), serial: 10}, pki.intermediate)
	other := makeCert(t, certSpec{cn: "other", notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), serial: 11}, pki.intermediate)
	c := tlsCertCollector(t, model.TLSConfig{CAFile: pki.rootFile, ServerName: "localhost"})
	chain := []*testCert{leaf, pki.intermediate}
	doc := func(staple []byte) tlsCertOCSP {
		t.Helper()
		address, _ := tlsServerStapling(t, chain, nil, true, staple)
		d, err := handshakeDoc(t, c, address)
		if err != nil {
			t.Fatal(err)
		}
		return d.OCSP
	}

	got := doc(ocspResponse(t, pki.intermediate, pki.intermediate, leaf, ocsp.Good, pki.now))
	// The responder writes its own time as produced_at.
	want := tlsCertOCSP{Stapled: true, Status: "good", Verified: true, ThisUpdate: pki.now.Add(-time.Hour).Unix(), NextUpdate: pki.now.Add(48 * time.Hour).Unix()}
	if got.ProducedAt < pki.now.Add(-2*time.Minute).Unix() || got.SecondsUntilNextUpdate < 47*3600 {
		t.Errorf("good: produced at %d, %d seconds until the next update", got.ProducedAt, got.SecondsUntilNextUpdate)
	}
	got.ProducedAt, got.SecondsUntilNextUpdate = 0, 0
	if got != want {
		t.Errorf("good: %+v\nwant %+v", got, want)
	}

	got = doc(ocspResponse(t, pki.intermediate, pki.intermediate, leaf, ocsp.Revoked, pki.now))
	if got.Status != "revoked" || !got.Verified || got.RevokedAt != pki.now.Add(-2*time.Hour).Unix() || got.RevocationReason != "keyCompromise" || got.SecondsUntilNextUpdate <= 0 {
		t.Errorf("revoked: %+v", got)
	}

	// Signed with a key that is not the issuer's.
	impostor := makeCert(t, certSpec{cn: "Test Intermediate", notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), ca: true, serial: 3}, nil)
	got = doc(ocspResponse(t, pki.intermediate, impostor, leaf, ocsp.Good, pki.now))
	if got.Status != "good" || got.Verified || got.Error == "" {
		t.Errorf("signed by another: %+v", got)
	}
	got = doc(ocspResponse(t, pki.intermediate, pki.intermediate, other, ocsp.Good, pki.now))
	if got.Verified || !strings.HasPrefix(got.Error, "the stapled response is not verified: ") {
		t.Errorf("of another certificate: %+v", got)
	}
	if got := doc([]byte("no response")); !got.Stapled || got.Status != "" || got.Verified || got.Error == "" {
		t.Errorf("not a response: %+v", got)
	}
	if got := doc(nil); got != (tlsCertOCSP{}) {
		t.Errorf("none stapled: %+v", got)
	}

	address, _ := tlsServerStapling(t, chain, nil, true, ocspResponse(t, pki.intermediate, pki.intermediate, leaf, ocsp.Revoked, pki.now))
	ctx, trace := WithSpanTrace(t.Context())
	if _, err := FetchCollector(ctx, address, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	facts := trace.Requests()[0].Facts
	if !slices.Contains(facts, boolFact("tlscert.ocsp.stapled", true)) || !slices.Contains(facts, nameFact("tlscert.ocsp.status", "revoked")) {
		t.Errorf("facts %+v", facts)
	}
}
