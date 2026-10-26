//go:build request_type_ldap || request_type_redis || request_type_s3file || request_type_sshexec || request_type_tcp

package fetch

import (
	"slices"
	"testing"
)

// checkOptInRequestKeys checks optInRequestKeys and optInTargetKeys of the
// opt-in type name in a build with it: each key listed is the type's and no
// other built type's, and, in a build with every type of the default build,
// each key of the type's that no other built type accepts is listed.
func checkOptInRequestKeys(t *testing.T, name string) {
	t.Helper()
	checkOptInKeys(t, name, "optInRequestKeys", optInRequestKeys[name], func(rt *RequestType) []string { return rt.Fields })
	checkOptInKeys(t, name, "optInTargetKeys", optInTargetKeys[name], func(rt *RequestType) []string { return rt.TargetFields })
}

// checkOptInKeys checks listed, the list called list, against the keys of
// name's that keys picks of every built type.
func checkOptInKeys(t *testing.T, name, list string, listed []string, keys func(*RequestType) []string) {
	t.Helper()
	own := RequestTypes[name]
	everyDefault := true
	for _, other := range knownRequestTypes {
		if !slices.Contains(optInRequestTypes, other) && RequestTypes[other] == nil {
			everyDefault = false
		}
	}
	for _, key := range keys(own) {
		var others []string
		for other, rt := range RequestTypes {
			if other != name && slices.Contains(keys(rt), key) {
				others = append(others, other)
			}
		}
		switch {
		case slices.Contains(listed, key) && len(others) > 0:
			t.Errorf("%s lists %s of %s, which %v accept too", list, key, name, others)
		case !slices.Contains(listed, key) && len(others) == 0 && everyDefault:
			t.Errorf("%s accepts %s, which no other type does, and %s does not list it", name, key, list)
		}
	}
	for _, key := range listed {
		if !slices.Contains(keys(own), key) {
			t.Errorf("%s lists %s, which %s does not accept", list, key, name)
		}
	}
}
