package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// A jq or yq label expression gives a value of any JSON type, and the label is
// its text. Go's %v is the wrong text for most of them: decoded JSON numbers
// are float64, which %v writes in exponent form from a million up, so an ID
// of 1234567 became the label "1.234567e+06", and an object or an array
// became Go syntax, "map[a:1]" or "[x y]". labelText writes a value as a
// person would read it, and refuses one that is not a single value.

// labelText is the text of the label value v. An object or an array is an
// error: a label is one value, and the expression should select a field of
// it, or join it into one with join(",").
func labelText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return labelNumber(x), nil
	case float32:
		return labelNumber(float64(x)), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case json.Number:
		return x.String(), nil
	case *big.Int:
		// gojq's integers beyond int's range.
		return x.String(), nil
	case map[string]any:
		return "", errors.New("is an object, not a single value; select one of its fields")
	case []any:
		return "", fmt.Errorf("is an array of %d values, not a single value; select one, or join them with join(\",\")", len(x))
	default:
		return fmt.Sprint(x), nil
	}
}

// labelNumber writes a number as JSON and JavaScript do: without an exponent
// from a millionth up to 1e21, so integers such as IDs and ports read as
// written, and with one beyond, where the digits would be mostly zeros.
// Non-finite values are written as Prometheus writes them.
func labelNumber(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "+Inf"
	case math.IsInf(x, -1):
		return "-Inf"
	case x == 0:
		// Negative zero too, which would otherwise be "-0".
		return "0"
	}
	if abs := math.Abs(x); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}
