package fetch

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// When a request fails, Go's HTTP client names the URL before the reason:
// `Get "http://db.internal/status": dial tcp 10.0.0.7:80: connect:
// connection refused`. The scraper can make the URL long, since a probe's
// target and its path parameters can be eight kilobytes each. So can a
// target: after a redirect, the request is for the URL its Location named,
// and a megabyte of headers can hold a megabyte of URL. When an error
// leaves the trip, it is cut at the end to model.MaxFailureBytes. For such
// an error, the cut-off end is the reason, the one part that is not the
// URL.
//
// So when a URL an error quotes is longer than shownURLBytes, it is shown
// by its start and its length (shortURLErrors), as a decoder shows a long
// body value, and the rest of the error stays.

// shownURLBytes is how much of a URL a failed fetch's error shows: enough
// for the scheme, the host and the start of the path of any URL, and for
// nearly every URL whole.
const shownURLBytes = 512

// shortURLErrors returns err with every URL quoted by a *url.Error in its
// chain shown by its first shownURLBytes bytes and its length, if it is
// longer. This applies where the error quotes it and where a wrapping error
// wrote the same URL again, as a redirect refusal does. The URL is looked
// for both as it is and as RedactURLErrors left it. Wrapping errors have
// already written the URL into their text, so the text is rewritten as a
// whole, as RedactURLErrors does. The result unwraps to err. It is
// recognised by the same text with the mark in place of each length
// (model.SameFailureAs), so the same failure with a longer URL that starts
// the same way is one failure in the log. A nil err, or one that quotes no
// long URL, is returned unchanged.
func shortURLErrors(err error) error {
	if err == nil {
		return nil
	}
	var said, recognised []string
	cut := func(written string) {
		if len(written) <= shownURLBytes {
			return
		}
		head := written[:model.HeadOf(written, shownURLBytes)]
		said = append(said, strconv.Quote(written), strconv.Quote(head)+model.CutMark(len(written), false), written, head+model.CutMark(len(written), false))
		recognised = append(recognised, strconv.Quote(written), strconv.Quote(head)+model.CutMark(len(written), true), written, head+model.CutMark(len(written), true))
	}
	walkErrors(err, func(e error) {
		urlErr, ok := e.(*url.Error) //nolint:errorlint // walkErrors visits each error of the chain itself
		if !ok || len(urlErr.URL) <= shownURLBytes {
			return
		}
		cut(urlErr.URL)
		if shown := RedactURLString(urlErr.URL, MaskQueryValues); shown != urlErr.URL {
			cut(shown)
		}
	})
	if len(said) == 0 {
		return err
	}
	text := err.Error()
	rewrite := strings.NewReplacer(said...)
	short := rewrite.Replace(text)
	if short == text {
		return err
	}
	return model.SameFailureAs(&redactedError{text: short, err: err, rewrite: rewrite}, strings.NewReplacer(recognised...).Replace(model.SameFailureText(err)))
}
