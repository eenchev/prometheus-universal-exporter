//go:build !select_request_types

package fetch

import (
	"reflect"
	"slices"
	"sort"
	"testing"
)

// Tests run in a default build, which carries every type but the opt-in
// ones, and in a default build with opt-in types added: it registers every
// type that is not opt-in, whichever opt-in types it has.
func TestADefaultBuildRegistersEveryRequestType(t *testing.T) {
	var known, built []string
	for _, name := range knownRequestTypes {
		if !slices.Contains(optInRequestTypes, name) {
			known = append(known, name)
		}
	}
	for _, name := range BuiltRequestTypes() {
		if !slices.Contains(optInRequestTypes, name) {
			built = append(built, name)
		}
	}
	sort.Strings(known)
	if !reflect.DeepEqual(built, known) {
		t.Fatalf("built %v, want every known type that is not opt-in, %v", built, known)
	}
}
