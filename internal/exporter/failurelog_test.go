package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// failedByText is failureLog.failedFor as it was while a failure was told
// from another by its error's text as it is.
func (f *failureLog) failedByText(logger *slog.Logger, level slog.Level, key, msg, stage string, err error, attrs ...any) {
	errText := ""
	if err != nil {
		errText = err.Error()
		attrs = append(attrs, "error", err)
	}
	now := f.now()
	f.mu.Lock()
	f.sweepLocked(now)
	st := f.entries[key]
	if st != nil && now.Sub(st.seen) > failureLogForget {
		delete(f.entries, key)
		st = nil
	}
	if st == nil || st.stage != stage || st.err != errText {
		if st != nil || f.rememberLocked() {
			f.entries[key] = &failureState{stage: stage, err: errText, first: now, logged: now, seen: now, failures: 1}
		}
		f.mu.Unlock()
		logger.Log(context.Background(), level, msg, attrs...)
		return
	}
	st.failures++
	st.seen = now
	if now.Sub(st.logged) < f.interval {
		st.suppressed++
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "repeat", true)...)
		return
	}
	repeated, since := st.suppressed+1, st.first
	st.suppressed, st.logged = 0, now
	f.mu.Unlock()
	logger.Log(context.Background(), level, msg, append(attrs, "repeated", repeated, "failing_since", since.UTC().Format(time.RFC3339))...)
}

// A failure whose error names no place in the response and no measured size
// is told from another as it was, by its text: over 20,000 failures and
// recoveries of three things in two stages — errors plain, wrapped, marked
// with their kind and none at all, minutes and hours apart — the log writes
// line for line what it wrote when it compared the texts.
func TestTheFailureLogWritesWhatItDidForErrorsThatNameNoPlace(t *testing.T) {
	random := rand.New(rand.NewSource(13))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	logs := func() (*failureLog, *bytes.Buffer, *slog.Logger) {
		f, out := newFailureLog(), &bytes.Buffer{}
		f.now = func() time.Time { return now }
		return f, out, slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
	}
	is, isOut, isLogger := logs()
	was, wasOut, wasLogger := logs()
	refused := errors.New("connection refused")
	failures := []error{
		nil,
		refused,
		errors.New("received HTTP status 503"),
		errors.New("received HTTP status 500"),
		fmt.Errorf("HTTP request failed: %w", refused),
		fmt.Errorf("HTTP request failed: %w (the probe ran out of its 10s budget)", context.DeadlineExceeded),
		model.MarkError(errors.New("response size exceeds limit 1024"), model.ErrLimitExceeded),
		model.MarkError(fmt.Errorf("metric %q value is missing: jq expression %q returned nothing", "m", ".a"), model.ErrMissingValue),
		model.JoinProblems(refused, io.ErrUnexpectedEOF),
		errors.Join(refused, io.EOF),
		errors.New(`CSV column "used" is empty in row 3`),
		errors.New(`CSV column "used" is empty in row 7`),
	}
	keys := []string{failureKey("web", "http://a", ""), failureKey("web", "http://b", ""), failureKey("files", "", "a.prom")}
	stages := []string{"http", "transform"}
	for range 20000 {
		key := keys[random.Intn(len(keys))]
		if random.Intn(20) == 0 {
			is.recovered(isLogger, key, "probe recovered", "collector", "web")
			was.recovered(wasLogger, key, "probe recovered", "collector", "web")
		} else {
			// Runs of one failure, as a target that is down makes them.
			err, stage := failures[random.Intn(len(failures))], stages[random.Intn(len(stages))]
			for range 1 + random.Intn(4) {
				is.failed(isLogger, slog.LevelError, key, "probe failed", stage, err, "collector", "web", "stage", stage)
				was.failedByText(wasLogger, slog.LevelError, key, "probe failed", stage, err, "collector", "web", "stage", stage)
				now = now.Add(time.Duration(random.Intn(150)) * time.Second)
			}
		}
		if random.Intn(400) == 0 {
			now = now.Add(2 * time.Hour)
		}
	}
	if isOut.String() != wasOut.String() {
		isLines, wasLines := bytes.Split(isOut.Bytes(), []byte("\n")), bytes.Split(wasOut.Bytes(), []byte("\n"))
		for i := range min(len(isLines), len(wasLines)) {
			if !bytes.Equal(isLines[i], wasLines[i]) {
				t.Fatalf("line %d is\n%s\nand was\n%s", i+1, isLines[i], wasLines[i])
			}
		}
		t.Fatalf("the log has %d lines, and had %d", len(isLines), len(wasLines))
	}
	if lines, repeats, sums := bytes.Count(isOut.Bytes(), []byte("\n")), bytes.Count(isOut.Bytes(), []byte(`"repeat":true`)), bytes.Count(isOut.Bytes(), []byte(`"repeated":`)); lines < 20000 || repeats < 2000 || sums < 200 {
		t.Fatalf("%d lines, %d repeats and %d sums of repeats: the generator shows too little", lines, repeats, sums)
	}
}
