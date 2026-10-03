//go:build !select_request_types

package fetch

import (
	"reflect"
	"sort"
	"testing"
)

// Tests run in a default build, which carries every type.
func TestADefaultBuildRegistersEveryRequestType(t *testing.T) {
	known := append([]string(nil), knownRequestTypes...)
	sort.Strings(known)
	if got := BuiltRequestTypes(); !reflect.DeepEqual(got, known) {
		t.Fatalf("built %v, want every known type %v", got, known)
	}
}
