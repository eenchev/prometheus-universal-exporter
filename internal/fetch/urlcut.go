package fetch

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Go's HTTP client says of a request that failed which URL it was for,
// before it says why: `Get "http://db.internal/status": dial tcp
// 10.0.0.7:80: connect: connection refused`. The URL is the scraper's to
// make long, a probe's target and its path parameters being eight kilobytes
// each, and a target's: the request a redirect led to is for the URL its
// Location named, of which a megabyte of headers holds a megabyte. An error
// is no longer than model.MaxFailureBytes when it leaves the trip, cut at
// the end, and of such an error the end is what was cut: the reason, the
// one part that is not the URL.
//
// So a URL an error quotes is shown by its start and its length when it is
// longer than shownURLBytes (shortURLErrors), as a decoder shows a long
// value of the body, and what the error says after it stays.

// shownURLBytes is how much of a URL a failed fetch's error shows: enough
// for the scheme, the host and the start of the path of any URL, and for
// nearly every URL whole.
const shownURLBytes = 512

// shortURLErrors is err with every URL a *url.Error of its chain quotes
// shown by its first shownURLBytes bytes and its length, when it is longer:
// where the error quotes it, and where an error that wraps it wrote the same
// URL again, as the refusal of a redirect does. The URL is looked for as it
// is and as RedactURLErrors left it. The errors that wrap one have written
// its URL into their text already, so the text is rewritten as a whole, as
// RedactURLErrors rewrites it; the result unwraps to err, and is recognised
// by the same text with the mark in place of each length
// (model.SameFailureAs), so the same failure of a longer URL that starts
// alike is one failure to the log. A nil err, or one that quotes no long
// URL, is returned as it is.
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
	short := strings.NewReplacer(said...).Replace(text)
	if short == text {
		return err
	}
	return model.SameFailureAs(&redactedError{text: short, err: err}, strings.NewReplacer(recognised...).Replace(model.SameFailureText(err)))
}
