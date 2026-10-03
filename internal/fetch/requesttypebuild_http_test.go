//go:build !select_request_types || request_type_http

package fetch

import (
	"testing"
)

func TestRegisteringARequestTypeTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate registration must panic")
		}
	}()
	registerRequestType(&RequestType{Name: RequestTypeHTTP})
}
