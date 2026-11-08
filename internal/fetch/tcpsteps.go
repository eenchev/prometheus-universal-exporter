//go:build !select_request_types || request_type_tcp

package fetch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A tcp collector's request.steps is a conversation: a protocol that needs
// more than one exchange before it says what a metric needs, as SMTP's
// EHLO, POP3's CAPA or IMAP's CAPABILITY after their greetings, or a
// command that only answers after another, as memcached's stats of a slab
// class. Each step's send is written, after the greeting and STARTTLS when
// the collector has them, and its answer read until what came of it matches
// the step's until; the last step without until reads until the server
// closes the connection. The answer the rules read is the steps' answers
// one after another, and with request.document each one's text, answer and
// seconds too.

// TCPStep is one step of request.steps: the text written, and the pattern
// its answer matches once it has all come.
type TCPStep struct {
	Send  string `yaml:"send"`
	Until string `yaml:"until,omitempty"`
}

// tcpMaxSteps bounds request.steps: a conversation of a few exchanges, not
// a session.
const tcpMaxSteps = 16

// validateTCPSteps checks request.steps: not beside send, send_hex,
// terminator, answer_bytes or udp, each of which is the one exchange's; at
// most tcpMaxSteps steps, each with a send, as send is held, and an until
// Go's regexp compiles, bounded as greeting is, with no placeholder, which
// every step but the last must have.
func validateTCPSteps(name string, r TCPConfig) error {
	if len(r.Steps) == 0 {
		return nil
	}
	for key, set := range map[string]bool{"send": r.Send != "", "send_hex": r.SendHex != "", "terminator": r.Terminator != "", "answer_bytes": r.AnswerBytes > 0, "udp": r.UDP} {
		if set {
			return fmt.Errorf("collector %q sets both request.steps and request.%s; with steps, each step has what it sends and where its answer ends", name, key)
		}
	}
	if len(r.Steps) > tcpMaxSteps {
		return fmt.Errorf("collector %q request.steps has %d steps, more than %d", name, len(r.Steps), tcpMaxSteps)
	}
	for i, step := range r.Steps {
		where := fmt.Sprintf("request.steps[%d]", i)
		switch {
		case step.Send == "":
			return fmt.Errorf("collector %q %s has no send; a step writes something, and the server's first words are its greeting", name, where)
		case len(step.Send) > tcpMaxText:
			return fmt.Errorf("collector %q %s.send is %d bytes, more than the %d a tcp collector may send", name, where, len(step.Send), tcpMaxText)
		case step.Until == "" && i < len(r.Steps)-1:
			return fmt.Errorf("collector %q %s has no until; every step but the last says by it when its answer has all come, as the next is written after it", name, where)
		case len(step.Until) > tcpMaxText:
			return fmt.Errorf("collector %q %s.until is %d bytes, more than the %d it may be", name, where, len(step.Until), tcpMaxText)
		case HasPathParams(step.Until):
			return fmt.Errorf("collector %q %s.until %q has a placeholder; the answer is matched against the pattern as it is written", name, where, step.Until)
		}
		if step.Until != "" {
			if _, err := tcpGreetingPattern(step.Until); err != nil {
				return fmt.Errorf("collector %q %s.until %q is not a regular expression Go reads: %w", name, where, step.Until, err)
			}
		}
		placeholders, err := (templateField{where + ".send", step.Send, "line"}).parse()
		if err != nil {
			return fmt.Errorf("collector %q %w", name, err)
		}
		for _, p := range placeholders {
			if character, refused := lineValueRefused(p.Default); p.HasDefault && refused {
				return fmt.Errorf("collector %q %s.send: the default of %s holds %s; %s", name, where, p.Name, character, lineValueRule)
			}
		}
	}
	return nil
}

// tcpStepRendered is a step of a scrape: its send with the probe's values
// filled, and its until.
type tcpStepRendered struct {
	send, until string
}

// tcpRenderSteps are the collector's steps with the probe's values filled.
func tcpRenderSteps(c *model.Collector, overrides RequestOverrides) ([]tcpStepRendered, error) {
	steps := tcpRequest(c).Steps
	out := make([]tcpStepRendered, len(steps))
	for i, step := range steps {
		send, err := templateField{fmt.Sprintf("request.steps[%d].send", i), step.Send, "line"}.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		out[i] = tcpStepRendered{send: send, until: step.Until}
	}
	return out, nil
}

// tcpShownSteps is the steps as a debug report shows them, each send
// quoted, one after another.
func tcpShownSteps(steps []tcpStepRendered) string {
	shown := make([]string, len(steps))
	for i, step := range steps {
		shown[i] = tcpShown(step.send)
	}
	return strings.Join(shown, " then ")
}

// tcpStepRead is what a step wrote and read, and how long it took.
type tcpStepRead struct {
	send   string
	answer []byte
	took   time.Duration
}

// tcpStepDocument is a step as request.document holds it.
type tcpStepDocument struct {
	Send    string  `json:"send"`
	Answer  string  `json:"answer"`
	Seconds float64 `json:"seconds"`
}

// tcpConverse speaks the steps on conn, reading through reader, each
// answer held, with those before it and the shared bytes of the greeting,
// to limit; x gets each step, the body, their answers one after another,
// and how reading stopped.
func tcpConverse(ctx context.Context, conn net.Conn, reader *bufio.Reader, address tcpAddress, steps []tcpStepRendered, limit, shared int64, x *tcpExchanged) error {
	var body []byte
	for i, step := range steps {
		began := time.Now()
		if _, err := io.WriteString(conn, step.send); err != nil {
			return tcpFailure(ctx, fmt.Errorf("sending request.steps[%d] to %s: %w", i, address.hostPort, err))
		}
		answer, err := tcpReadStep(ctx, reader, address, i, step.until, limit, shared+int64(len(body)))
		if err != nil {
			return err
		}
		x.conversation = append(x.conversation, tcpStepRead{send: step.send, answer: answer, took: time.Since(began)})
		body = append(body, answer...)
	}
	x.body = body
	x.how = fmt.Sprintf("%d steps, %d bytes read", len(steps), len(body))
	return nil
}

// tcpReadStep reads the answer of the step at i until it matches until, or
// without one until the server closes the connection, holding it, with
// the used bytes before it, to limit.
func tcpReadStep(ctx context.Context, reader *bufio.Reader, address tcpAddress, i int, until string, limit, used int64) ([]byte, error) {
	var pattern interface{ Match([]byte) bool }
	if until != "" {
		p, err := tcpGreetingPattern(until)
		if err != nil {
			return nil, fmt.Errorf("request.steps[%d].until %q: %w", i, until, err)
		}
		pattern = p
	}
	var answer []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			if used+int64(len(answer))+int64(n) > limit {
				return nil, model.MarkError(model.Errorf("the answers of %s to request.steps, to the step at %d, are larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", address.hostPort, i, model.Size(limit)), model.ErrLimitExceeded)
			}
			answer = append(answer, chunk[:n]...)
			if pattern != nil && pattern.Match(answer) {
				return answer, nil
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF) && pattern == nil:
			return answer, nil
		case errors.Is(err, io.EOF):
			return nil, model.Errorf("the server closed the connection after %d bytes of the answer to request.steps[%d], %s, which does not match its until %q", len(answer), i, model.Quoted(answer), until)
		case pattern != nil:
			return nil, tcpFailure(ctx, model.Errorf("reading from %s: the answer to request.steps[%d], %d bytes, %s, did not come to match its until %q: %w", address.hostPort, i, len(answer), model.Quoted(answer), until, err))
		}
		return nil, tcpFailure(ctx, fmt.Errorf("reading from %s: the server kept the connection open after the last step, %d bytes read; a last step without until reads until the server closes it: %w", address.hostPort, len(answer), err))
	}
}
