package fetch

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every request type of the build says what a target of it is, or that it
// takes none, and what a pasted response of it is, as the collectors page
// and the debug page ask for them (RequestType.TargetHint, NoTarget,
// AnswerHint): a type left out of a build takes its words with it, and only
// a type whose target is a URL hints one.
func TestEveryBuiltRequestTypeSaysWhatItsTargetAndItsAnswerAre(t *testing.T) {
	for _, name := range BuiltRequestTypes() {
		rt := RequestTypes[name]
		if rt.AnswerHint == "" {
			t.Errorf("%s says nothing of a pasted response", name)
		}
		c := &model.Collector{Name: "x", Request: model.RequestConfig{Type: name}}
		prompt := TargetPromptOf(c)
		switch {
		case rt.NoTarget:
			if !prompt.None || prompt.Label != "" || prompt.Hint != "" {
				t.Errorf("%s takes no target, and is prompted %+v", name, prompt)
			}
			continue
		case prompt.None || prompt.Label == "" || prompt.Hint == "":
			t.Errorf("%s says no target: %+v", name, prompt)
		}
		urls := strings.Contains(prompt.Hint, "http://") || strings.Contains(prompt.Hint, "https://")
		if urls != (name == RequestTypeHTTP || name == RequestTypeGraphite) {
			t.Errorf("%s hints the target %q", name, prompt.Hint)
		}
	}
}

// A request type registered without saying what its target is, nor that it
// takes none, or without saying what a pasted response of it is, does not
// build: registration panics before the type is in RequestTypes.
func TestARequestTypeMustSayWhatItsTargetAndItsAnswerAre(t *testing.T) {
	hint := func(*model.Collector) (string, string) { return "Server", "host:port" }
	for name, rt := range map[string]*RequestType{
		"test_no_target_hint": {Requester: hintlessRequester{}, AnswerHint: "Paste it"},
		"test_no_answer_hint": {Requester: hintlessRequester{}, TargetHint: hint},
		"test_no_target":      {Requester: hintlessRequester{}, NoTarget: true},
	} {
		rt.Name = name
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s registered", name)
				}
			}()
			registerRequestType(rt)
		}()
		if RequestTypes[name] != nil {
			delete(RequestTypes, name)
			delete(registered, name)
			t.Errorf("%s is in RequestTypes", name)
		}
	}
}

// hintlessRequester is a Requester that does nothing.
type hintlessRequester struct{ URLTarget }

func (hintlessRequester) Validate(*model.Collector) error { return nil }

func (hintlessRequester) Fetch(context.Context, string, *model.Collector, RequestOverrides, http.Header) (*HTTPResponse, error) {
	return nil, nil
}

func (hintlessRequester) CheckTarget(*model.Collector, string, bool) error { return nil }
