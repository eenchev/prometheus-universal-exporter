//go:build !select_request_types || request_type_http

package fetch

import (
	"errors"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The keys of the other request types do not apply to an http collector,
// which is refused by the key's name, whether or not the build has the type
// the key belongs to. The tests of each type's own validation have what a
// collector of the type must be: TestGraphiteRequestValidation,
// TestGraphiteStaticTargetRequest, TestGRPCValidation, TestGRPCAcceptedCodes
// and TestLocalDirectoryValidation.
func TestTheKeysOfOtherTypesDoNotApplyToHTTP(t *testing.T) {
	t.Run("graphite's targets", func(t *testing.T) {
		// The graphite keys are graphite's alone.
		h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Targets: []string{"a"}}}
		if err := ValidateRequest(&h); err == nil || !strings.Contains(err.Error(), `request.targets, which does not apply to request.type "http"`) {
			t.Fatalf("an http collector with targets: err=%v", err)
		}
	})
	t.Run("graphite's window on a static target", func(t *testing.T) {
		// A static target of an http collector cannot set them.
		h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
		if err := ValidateRequest(&h); err != nil {
			t.Fatal(err)
		}
		if err := CheckTargetRequest(&model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{From: "-1h"}}, &h); err == nil || !strings.Contains(err.Error(), `request.from, which does not apply to collector "web"`) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("grpc's retry.codes", func(t *testing.T) {
		// retry.codes is grpc's alone.
		h := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Codes: []string{"UNAVAILABLE"}}}}
		if err := ValidateRequest(&h); err == nil || !strings.Contains(err.Error(), "applies only to request.type grpc") {
			t.Fatalf("an http collector took retry.codes: %v", err)
		}
	})
	t.Run("grpc's accept_codes", func(t *testing.T) {
		http := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptCodes: []string{"NOT_FOUND"}}}
		if err := ValidateRequest(&http); err == nil || !strings.Contains(err.Error(), "does not apply") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("localfile's files", func(t *testing.T) {
		c := testutil.Collector("web", "text")
		c.Request.Files = []string{"*"}
		err := ValidateRequest(&c)
		if want := "request.files, which does not apply"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want %q", err, want)
		}
	})
}

// An http probe needs a target, where a localfile probe may leave it out
// (TestLocalFileTargetInterpretation).
func TestAnHTTPProbeWithoutATargetIsRefused(t *testing.T) {
	if !errors.Is(CheckTarget(ptr(testutil.Collector("web", "text")), "", false), ErrMissingTarget) {
		t.Error("an http probe without a target must be refused")
	}
}

func ptr[T any](v T) *T { return &v }
