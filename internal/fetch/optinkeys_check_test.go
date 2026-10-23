//go:build request_type_redis || request_type_s3file

package fetch

import (
	"slices"
	"testing"
)

// checkOptInRequestKeys checks optInRequestKeys of the opt-in type name in
// a build with it: each key listed is the type's and no other built type's,
// and, in a build with every type of the default build, each key of the
// type's that no other built type accepts is listed.
func checkOptInRequestKeys(t *testing.T, name string) {
	t.Helper()
	own := RequestTypes[name]
	listed := optInRequestKeys[name]
	everyDefault := true
	for _, other := range knownRequestTypes {
		if !slices.Contains(optInRequestTypes, other) && RequestTypes[other] == nil {
			everyDefault = false
		}
	}
	for _, key := range own.Fields {
		var others []string
		for other, rt := range RequestTypes {
			if other != name && slices.Contains(rt.Fields, key) {
				others = append(others, other)
			}
		}
		switch {
		case slices.Contains(listed, key) && len(others) > 0:
			t.Errorf("optInRequestKeys lists %s of %s, which %v accept too", key, name, others)
		case !slices.Contains(listed, key) && len(others) == 0 && everyDefault:
			t.Errorf("%s accepts %s, which no other type does, and optInRequestKeys does not list it", name, key)
		}
	}
	for _, key := range listed {
		if !slices.Contains(own.Fields, key) {
			t.Errorf("optInRequestKeys lists %s, which %s does not accept", key, name)
		}
	}
}
