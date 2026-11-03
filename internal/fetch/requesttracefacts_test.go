//go:build !select_request_types || request_type_dns || request_type_grpc || request_type_ldap || request_type_ntp || request_type_redis || request_type_s3file || request_type_sql || request_type_sshfile || request_type_tlscert

package fetch

import (
	"strconv"
	"testing"
)

// factsOf are a request's facts by key, each value written as its kind
// is: an integer in decimal, a boolean as true or false, a floating-point
// number as strconv writes it shortest, a name as it is. A key told twice
// fails the test.
func factsOf(t *testing.T, r TracedRequest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range r.Facts {
		if _, twice := out[f.Key]; twice {
			t.Errorf("%s is told twice in %+v", f.Key, r.Facts)
		}
		switch f.Kind {
		case TraceFactBool:
			out[f.Key] = strconv.FormatBool(f.Bool)
		case TraceFactDouble:
			out[f.Key] = strconv.FormatFloat(f.Double, 'g', -1, 64)
		case TraceFactName:
			out[f.Key] = f.Name
		default:
			out[f.Key] = strconv.FormatInt(f.Int, 10)
		}
	}
	return out
}

// requireFacts fails unless facts are exactly want.
func requireFacts(t *testing.T, what string, facts, want map[string]string) {
	t.Helper()
	if len(facts) != len(want) {
		t.Errorf("%s tells %v, want %v", what, facts, want)
		return
	}
	for key, value := range want {
		if facts[key] != value {
			t.Errorf("%s tells %v, want %v", what, facts, want)
			return
		}
	}
}
