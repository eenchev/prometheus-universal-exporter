package fetch

import (
	"errors"
	"net"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A failed fetch's error names addresses of the exporter's side of the
// network as Go's packages write them: the address and port a connection was
// made from (`read tcp 10.0.0.7:51234->10.1.2.3:443`), the proxy a request
// went through (`proxyconnect tcp: dial tcp 10.0.0.5:3128`, everything a
// connection to it failed with, and a proxy's words in refusing a tunnel,
// ProxyRefusedError), the name
// server a lookup asked (`lookup db on 10.96.0.10:53`), and, in the text of
// a resolver's exchange or a gRPC status, the same source address. Those are
// the operator's: the pod's address, the proxy's and the cluster's resolver
// are no part of what a probe's caller asked for. So the error is marked as
// the operator's (model.WithOperatorDetail), and the caller is told it with
// each of them withheld (model.WithheldMark); the address the connection
// went to, which the caller named, stays. The log, a debug report and, with
// --web.debug=full, the status page have the whole text.
//
// The addresses are read from the errors' types, as sameFetchFailure reads
// them, and from the texts only in the forms those packages write.

// withTransportWithheld is err marked as the operator's when it names an
// address of the exporter's side of the network, as above; err as it is
// otherwise, and nil for nil.
func withTransportWithheld(err error) (marked error) {
	if err == nil {
		return nil
	}
	// As sameFetchFailure: reading an error must never take a probe down.
	defer func() {
		if recover() != nil {
			marked = err
		}
	}()
	var parts []string
	walkErrors(err, func(e error) { parts = appendTransportParts(parts, e) })
	if len(parts) == 0 {
		return err
	}
	told := model.CallerText(err)
	shown := told
	for i := 0; i < len(parts); i += 2 {
		shown = strings.ReplaceAll(shown, parts[i], parts[i+1])
	}
	if shown == told {
		return err
	}
	return model.WithOperatorDetail(err, shown)
}

// appendTransportParts adds to parts what e, one error of a chain, wrote of
// the exporter's side of the network, and what a caller is told in its
// place: the two as a pair.
func appendTransportParts(parts []string, e error) []string {
	add := func(written, told string) {
		if written != "" && written != told {
			parts = append(parts, written, told)
		}
	}
	withheld := func(text string) string {
		return strings.ReplaceAll(withoutSourceAddresses(text), model.MovingMark, model.WithheldMark)
	}
	switch e := e.(type) { //nolint:errorlint // walkErrors hands over each error of the chain itself
	case *net.OpError:
		if e == nil {
			break
		}
		if e.Source != nil {
			// As (*net.OpError).Error writes it, as sameFetchFailure reads it.
			if source := e.Source.String(); e.Addr != nil {
				add(" "+source+"->", " "+model.WithheldMark+"->")
			} else {
				add(" "+source+": ", " "+model.WithheldMark+": ")
			}
		}
		// Everything a connection to the proxy failed with is of the
		// proxy: its address, the name it is reached by in a failed
		// lookup, the names of an https proxy's certificate. So the
		// whole of it is withheld, and, should the text have been
		// rewritten past matching, the address of the dial at least.
		if e.Op == "proxyconnect" && e.Err != nil {
			prefix := "proxyconnect " + e.Net + ": "
			add(prefix+e.Err.Error(), prefix+model.WithheldMark)
			var dial *net.OpError
			if errors.As(e.Err, &dial) && dial != nil && dial.Addr != nil {
				add(" "+dial.Addr.String(), " "+model.WithheldMark)
			}
		}
	case *net.DNSError:
		if e != nil {
			if e.Server != "" {
				add(" on "+e.Server+":", " on "+model.WithheldMark+":")
			}
			add(e.Err, withheld(e.Err))
		}
	case *ProxyRefusedError:
		// What the proxy said in refusing the tunnel is its own.
		if e != nil {
			add(": "+e.Status, ": "+model.WithheldMark)
		}
	case *CallStatusError:
		if e != nil {
			add(e.Message, withheld(e.Message))
		}
	}
	return parts
}
