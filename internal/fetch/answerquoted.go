package fetch

import (
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// goQuotesTheAnswer are the words Go's HTTP client puts before what it
// quotes of a server's reply that is no HTTP answer: its first line, a
// status code, a version or a header line (net/http's response.go,
// net/textproto's reader.go). That line tells apart a target that speaks
// another protocol, such as an SSH or SMTP banner or a Redis error. A
// probe's caller who names such a target is not told what it said.
var goQuotesTheAnswer = []string{
	"malformed HTTP response ",
	"malformed HTTP status code ",
	"malformed HTTP version ",
	"malformed MIME header initial line: ",
	"malformed MIME header line: ",
	"malformed MIME header: missing colon: ",
}

// withAnswerWithheld is err, an error of an HTTP request, marked as the
// operator's when Go's words in it quote what the server sent. A probe's
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
