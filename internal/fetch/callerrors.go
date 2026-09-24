package fetch

import (
	"errors"
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The errors a fetch can end with that say more than that it failed. They
// live outside every request type's build tag, and import nothing of any
// type's, so the exporter reads them in any build: a build without the grpc
// type simply never produces a CallStatusError.

// StageError is a fetch that failed before the target was asked, at a stage
// of its own, such as a grpc message that does not fit the method's input
// type: the stage is "message" rather than the type's fetch stage.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Err.Error() }

func (e *StageError) Unwrap() error { return e.Err }

// CallStatusError is a gRPC call that ended with a status other than OK:
// Code is the status code's number, CodeName its name, such as UNAVAILABLE,
// and Message what the server, or the client library, said.
type CallStatusError struct {
	Code     int
	CodeName string
	Message  string
	// Err is what the error wraps: a context error when the deadline or a
	// cancellation ended the call, the limit error when the answer was too
	// large, or nil.
	Err error
}

func (e *CallStatusError) Error() string {
	if e.Message == "" {
		return "gRPC call failed: " + e.CodeName
	}
	return fmt.Sprintf("gRPC call failed: %s: %s", e.CodeName, e.Message)
}

func (e *CallStatusError) Unwrap() error { return e.Err }

// CallAnswerError is a gRPC call answered OK whose answer could not be used,
// such as one over the response limit once rendered as JSON. The call's
// status was OK, which GRPCStatusCode reports.
type CallAnswerError struct{ Err error }

func (e *CallAnswerError) Error() string { return e.Err.Error() }

func (e *CallAnswerError) Unwrap() error { return e.Err }

// FetchErrorStage names the stage a failed fetch is reported under: the
// stage a StageError names, else the type's fetch stage.
func FetchErrorStage(c *model.Collector, err error) string {
	var stage *StageError
	if errors.As(err, &stage) && stage.Stage != "" {
		return stage.Stage
	}
	return FetchStage(c)
}

// GRPCStatusCode is the gRPC status code a fetch of a grpc collector ended
// with: 0 when err is nil, the code of a call that failed, and -1 when no
// call was made, as when the message did not encode. ok reports whether the
// collector is a grpc one at all.
func GRPCStatusCode(c *model.Collector, err error) (code int, ok bool) {
	if c.Request.Type != RequestTypeGRPC {
		return 0, false
	}
	if err == nil {
		return 0, true
	}
	var status *CallStatusError
	if errors.As(err, &status) {
		return status.Code, true
	}
	var answered *CallAnswerError
	if errors.As(err, &answered) {
		return 0, true
	}
	return -1, true
}
