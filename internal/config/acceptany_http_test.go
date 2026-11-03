//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"
)

// request.accept_status: [any] is read as it is written, in the
// configuration, a collector file and the static target file, and is
// refused beside another entry and in another case, as the schemas refuse it.
func TestAcceptStatusAnyIsReadAloneInEveryFile(t *testing.T) {
	for file, load := range acceptStatusLoads(t, "any") {
		if load.err != nil || !slices.Equal(load.statuses, []string{"any"}) {
			t.Errorf("%s: accept_status: [any] is %q, %v", file, load.statuses, load.err)
		}
	}
	for written, want := range map[string]string{
		"any, 2xx": `request.accept_status entry "any" accepts every status from 100 to 599 and is written alone, without "2xx"`,
		"503, any": `request.accept_status entry "any" accepts every status from 100 to 599 and is written alone, without "503"`,
		"ANY":      `request.accept_status entry "ANY" is not an HTTP status`,
		`"Any"`:    `request.accept_status entry "Any" is not an HTTP status`,
	} {
		for file, load := range acceptStatusLoads(t, written) {
			if load.err == nil || !strings.Contains(load.err.Error(), want) {
				t.Errorf("%s: accept_status: [%s] is %q, %v; want %q", file, written, load.statuses, load.err, want)
			}
		}
	}
}
