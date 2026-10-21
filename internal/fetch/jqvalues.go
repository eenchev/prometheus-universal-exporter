//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

// What the jq expressions a request reads an answer with — request.pagination's
// next, and request.login's token and expires_in — are given of the answer,
// and how they name what they gave. Both types that make a request over
// HTTP have them, for a login; pagination is http's alone.

// jqHeaders are an answer's headers as jq's $headers has them: by lower-case
// name, each with its values joined by ", ", as the transform has them.
func jqHeaders(h http.Header) any {
	if h == nil {
		return nil
	}
	headers := make(map[string]any, len(h))
	for name, values := range h {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return headers
}

// jsonKind names the kind of a decoded JSON value for a message.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "text"
	case json.Number, int, float64, *big.Int:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("a %T", v)
}
