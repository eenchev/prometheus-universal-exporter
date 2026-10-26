package fetch

import (
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// goQuotesTheAnswer are the words before which Go's HTTP client quotes what
// a server sent that is no HTTP answer: the first line of it, a status code,
// a version or a header line (net/http's response.go, net/textproto's
// reader.go). A target that speaks another protocol — an SSH or SMTP
// banner, a Redis error — is told apart by that line, and a probe's caller
// who names such a target is not told what it said.
var goQuotesTheAnswer = []string{
	"malformed HTTP response ",
	"malformed HTTP status code ",
	"malformed HTTP version ",
	"malformed MIME header initial line: ",
	"malformed MIME header line: ",
	"malformed MIME header: missing colon: ",
}

// withAnswerWithheld is err, an error of an HTTP request, marked as the
// operator's when Go's words in it quote what the server sent: a probe's
// caller is told the text up to those words, and model.WithheldMark after
// them.
func withAnswerWithheld(err error) error {
	if err == nil {
		return nil
	}
	if told, quoted := toldOfGoQuoting(err.Error()); quoted {
		return model.WithOperatorDetail(err, told)
	}
	return err
}

// toldOfGoQuoting is text, an HTTP request's error, as a probe's caller is
// told it, and whether Go's words in it quote what the server sent.
func toldOfGoQuoting(text string) (string, bool) {
	for _, words := range goQuotesTheAnswer {
		if i := strings.Index(text, words); i >= 0 {
			return text[:i] + words + model.WithheldMark, true
		}
	}
	return "", false
}
