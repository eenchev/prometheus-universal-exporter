//go:build !select_request_types || request_type_graphite

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A Graphite server that resets every connection fails every fetch of a
// graphite collector with an error that names the port that connection was
// made from: the two errors read differently and are recognised by one
// text, as an http collector's are.
func TestAGraphiteServersResetIsRecognisedWhateverPortItWasMadeFrom(t *testing.T) {
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	target := resettingTarget(t)
	c := graphiteCollector("a.b")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	a, b := fetchFailures(t, &c, "http://"+target)
	if a.Error() == b.Error() || !strings.Contains(a.Error(), ": read tcp 127.0.0.1:") {
		t.Fatalf("the failures read\n%v\n%v\nwant each with the port its connection was made from", a, b)
	}
	same := model.SameFailureText(a)
	if same != model.SameFailureText(b) || !strings.HasSuffix(same, ": read tcp "+model.MovingMark+"->"+target+": read: connection reset by peer") {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant one text with the mark for the port", a, b, same, model.SameFailureText(b))
	}
}
