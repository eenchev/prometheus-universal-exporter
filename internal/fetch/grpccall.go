//go:build !select_request_types || request_type_grpc

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A call: the message is encoded from JSON into the input type, sent with
// the metadata and credentials, and the answer is rendered as JSON in the
// protobuf JSON mapping, with three choices made for metrics:
//
//   - Fields at their zero value are written, so a queue whose depth is 0
//     has "depth": 0, rather than no depth for a rule to report missing.
//   - Field names are as in the .proto file, include_shards rather than
//     includeShards, as the service's own documentation shows them.
//   - 64-bit integers are strings, as the mapping has it, which metric
//     values read as numbers.
//
// A call answered OK is an ordinary fetch result: status 200, the JSON as its
// body, and the answer's headers and trailers as its headers. Any other
// status fails the fetch with a CallStatusError.

// grpcJSON renders an answer.
var grpcJSON = protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}

// callGRPC makes one scrape's call, with its retries.
func callGRPC(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	address, err := parseGRPCTarget(target)
	if err != nil {
		return nil, err
	}
	secure, err := address.useTLS(c)
	if err != nil {
		return nil, err
	}
	key := grpcConnKey{dial: address.dial, tls: secure}
	if secure {
		key.settings = c.Request.TLS
		if overrides.InsecureSkipVerify != nil {
			key.settings.InsecureSkipVerify = *overrides.InsecureSkipVerify
		}
	}
	conn, err := grpcConns.get(key, time.Now())
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	message, err := grpcMessage(c, overrides)
	if err != nil {
		return nil, err
	}
	md, err := grpcOutgoing(c, overrides, forwarded)
	if err != nil {
		return nil, err
	}
	attempts := c.Request.Retry.Attempts
	if overrides.RetryAttempts != nil {
		attempts = *overrides.RetryAttempts
	}
	backoff := time.Duration(c.Request.Retry.Backoff)
	if overrides.RetryBackoff != nil {
		backoff = *overrides.RetryBackoff
	}
	retried := c.Request.Retry.Codes
	if overrides.RetryCodes != nil {
		retried = overrides.RetryCodes
	}
	if retried == nil {
		retried = grpcDefaultRetryCodes
	}
	limit := responseLimit(c)

	reflection := reflectedKey{conn: key, service: strings.SplitN(c.Request.RPC, "/", 2)[0]}
	method := func() (grpcMethod, error) {
		if c.Request.Descriptors != descriptorsReflection {
			return staticMethod(c)
		}
		files, err := reflectionAnswers.files(ctx, conn, reflection, time.Now())
		if err != nil {
			return grpcMethod{}, reflectionError(ctx, err)
		}
		return findMethod(files, c.Request.RPC, "the server's reflection answer")
	}
	encode := func() (grpcMethod, *dynamicpb.Message, error) {
		m, err := method()
		if err != nil {
			return grpcMethod{}, nil, err
		}
		in, err := m.encode(message)
		if err != nil {
			return grpcMethod{}, nil, &StageError{Stage: "message", Err: fmt.Errorf("request.message does not fit %s: %w", m.input(), err)}
		}
		return m, in, nil
	}
	m, in, err := encode()
	if err != nil {
		return nil, err
	}
	callCtx := metadata.NewOutgoingContext(ctx, md)
	start := time.Now()
	refreshed := false
	for attempt := 0; ; attempt++ {
		out := dynamicpb.NewMessage(m.desc.Output())
		var header, trailer metadata.MD
		err := conn.Invoke(callCtx, m.path(), in, out,
			grpc.MaxCallRecvMsgSize(int(min(limit, math.MaxInt32))),
			grpc.Header(&header), grpc.Trailer(&trailer))
		if err == nil {
			body, err := renderAnswer(out)
			if err != nil {
				return nil, &CallAnswerError{Err: err}
			}
			// The limit bounds the message as it arrived, and the JSON it
			// became, which writing every zero value can make much larger.
			if int64(len(body)) > limit {
				return nil, &CallAnswerError{Err: model.MarkError(fmt.Errorf("the answer is %d bytes as JSON, over the response limit of %d; raise request.max_response_bytes or limits.max_response_bytes", len(body), limit), model.ErrLimitExceeded)}
			}
			return &HTTPResponse{StatusCode: http.StatusOK, Headers: grpcHeaders(header, trailer), Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}, nil
		}
		st := status.Convert(err)
		// With reflection, a method the server does not know, or an answer
		// that does not decode, may be the server's schema having changed
		// since it was asked: it is asked again, once, and the call made
		// again. This is not one of the retries.
		if c.Request.Descriptors == descriptorsReflection && !refreshed && (st.Code() == codes.Unimplemented || undecodable(st)) && ctx.Err() == nil {
			refreshed = true
			reflectionAnswers.forget(reflection)
			if m, in, err = encode(); err != nil {
				return nil, err
			}
			attempt--
			continue
		}
		if attempt < attempts && slices.Contains(retried, grpcCodeName(st.Code())) && ctx.Err() == nil {
			if waitErr := waitRetry(ctx, backoff); waitErr != nil {
				return nil, fmt.Errorf("%w (the wait before retrying was cut short: %w)", callStatusError(ctx, st), waitErr)
			}
			continue
		}
		return nil, callStatusError(ctx, st)
	}
}

// renderAnswer renders an answer as compact JSON. The protobuf runtime
// varies the spaces it writes from build to build, so its output is
// compacted to be the same for the same answer.
func renderAnswer(out *dynamicpb.Message) ([]byte, error) {
	raw, err := grpcJSON.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("rendering the answer as JSON: %w", err)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, fmt.Errorf("rendering the answer as JSON: %w", err)
	}
	return b.Bytes(), nil
}

// grpcMessage is the JSON message a call sends: the probe's or the static
// target's, else the collector's with its placeholders filled in, else {}.
func grpcMessage(c *model.Collector, overrides RequestOverrides) (string, error) {
	message := c.Request.Message
	if overrides.Message != nil {
		message = *overrides.Message
	} else if HasPathParams(message) {
		rendered, err := templateField{"request.message", message, "body"}.render(overrides.Params)
		if err != nil {
			return "", err
		}
		message = rendered
	}
	if strings.TrimSpace(message) == "" {
		message = "{}"
	}
	return message, nil
}

// grpcOutgoing is the metadata a call sends: the collector's, a static
// target's own over it, the credentials as authorization, and the forwarded
// headers over all of them, their names lower-cased. A forwarded header gRPC
// or HTTP/2 reserves is not sent.
func grpcOutgoing(c *model.Collector, overrides RequestOverrides, forwarded http.Header) (metadata.MD, error) {
	md := metadata.MD{}
	values, err := renderedValues(c.Request.Metadata, "header", "request.metadata.", overrides.Params)
	if err != nil {
		return nil, err
	}
	for key, value := range values {
		md.Set(key, value)
	}
	for key, value := range overrides.Metadata {
		md.Set(key, value)
	}
	authorization, err := requestAuthorization(c)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		md.Set("authorization", authorization)
	}
	for name, values := range forwarded {
		key := strings.ToLower(name)
		if checkGRPCMetadataName(key) != nil || len(values) == 0 {
			continue
		}
		md.Set(key, values...)
	}
	return md, nil
}

// grpcHeaders are an answer's headers and trailers as a response's
// headers, the content type the JSON the answer became.
func grpcHeaders(header, trailer metadata.MD) http.Header {
	out := http.Header{}
	for _, md := range []metadata.MD{header, trailer} {
		for key, values := range md {
			if strings.HasSuffix(key, "-bin") {
				continue
			}
			for _, value := range values {
				out.Add(key, value)
			}
		}
	}
	out.Set("Content-Type", "application/json")
	return out
}

// grpcCodeName is a status code's name, as retry.codes names it.
func grpcCodeName(code codes.Code) string {
	if int(code) < len(grpcCodeNames) {
		return grpcCodeNames[code]
	}
	return fmt.Sprintf("CODE_%d", code)
}

// undecodable says the call failed because its answer did not decode into
// the output type.
func undecodable(st *status.Status) bool {
	return st.Code() == codes.Internal && strings.Contains(st.Message(), "unmarshal")
}

// callStatusError is the error of a call that ended with st. A call the
// deadline or a cancellation ended wraps that context error, and an answer
// over the size limit is counted as a limit.
func callStatusError(ctx context.Context, st *status.Status) error {
	e := &CallStatusError{Code: int(st.Code()), CodeName: grpcCodeName(st.Code()), Message: st.Message()}
	switch {
	case st.Code() == codes.ResourceExhausted && strings.Contains(st.Message(), "larger than max"):
		e.Err = model.ErrLimitExceeded
	case ctx.Err() != nil:
		e.Err = ctx.Err()
	}
	return e
}

// reflectionError is a failed reflection call as a call's error: one that
// ended with a status keeps its code, since the target answered.
func reflectionError(ctx context.Context, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	e := &CallStatusError{Code: int(st.Code()), CodeName: grpcCodeName(st.Code()), Message: "asking the reflection service: " + st.Message(), Err: ctx.Err()}
	return e
}
