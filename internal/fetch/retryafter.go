package fetch

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryWait is how long fetch waits before retrying an answer of a retried
// status, and why, for the debug report.
type retryWait struct {
	wait time.Duration
	why  string
	// giveUp is set when the answer's Retry-After asks for a wait that
	// would end at or after the request's deadline: the retry could not
	// be sent in time, so the answer is the request's answer at once, and
	// giveUp says why it was not retried.
	giveUp string
}

// maxRetryAfterSeconds bounds a Retry-After in seconds, so one too large for
// a time.Duration is still read as a wait, a very long one, rather than as a
// value that is not one.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// retryAfterWait is the wait before retrying an answer whose Retry-After
// header is header, under retry.retry_after: the delay it asks for, as a
// number of seconds or an HTTP date taken from now, else backoff. A date
// already past asks for no wait. A value that is neither, a negative number
// among them, is not the target's to be read, and backoff is waited as
// without the header. A wait that would end at or after ctx's deadline is
// not made at all.
func retryAfterWait(ctx context.Context, header string, backoff time.Duration, now time.Time) retryWait {
	value := strings.TrimSpace(header)
	if value == "" {
		return retryWait{wait: backoff, why: "retry.backoff; the answer has no Retry-After"}
	}
	wait, ok := parseRetryAfter(value, now)
	if !ok {
		return retryWait{wait: backoff, why: fmt.Sprintf("retry.backoff; its Retry-After %q is neither a number of seconds nor an HTTP date", shownRetryAfter(value))}
	}
	if deadline, has := ctx.Deadline(); has && wait > 0 {
		if left := deadline.Sub(now); wait >= left {
			return retryWait{giveUp: fmt.Sprintf("its Retry-After asks for %s, longer than the %s left of the probe's time", shownWait(wait), max(left, 0).Round(time.Millisecond))}
		}
	}
	return retryWait{wait: wait, why: "as Retry-After asked"}
}

// parseRetryAfter reads a Retry-After value (RFC 9110, section 10.2.3):
// delta-seconds, one or more digits, or an HTTP date.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds > maxRetryAfterSeconds {
			// Only digits, so too many of them: longer than any deadline.
			seconds = maxRetryAfterSeconds
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}

// shownWait is a Retry-After's wait for a message: a whole number of
// seconds as Go writes a duration, or "longer than a year" for one only a
// target that means "never" sends.
func shownWait(wait time.Duration) string {
	if wait > 365*24*time.Hour {
		return "longer than a year"
	}
	return wait.Round(time.Millisecond).String()
}

// shownRetryAfter is a header value the target chose, cut to a length a
// message can quote.
func shownRetryAfter(value string) string {
	const most = 64
	if len(value) > most {
		return strings.ToValidUTF8(value[:most], "") + "..."
	}
	return value
}
