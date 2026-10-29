package fetch

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// httpMethods are the methods an http collector and its static targets
// send.
var httpMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead}

// HTTPMethodSpellings are the ways request.method may be written, a
// collector's and a static target's: each of httpMethods in upper case, as
// HTTP names them, or in lower case. The schemas take these alone, and so
// does the exporter, rather than any case, which no schema enum can list.
func HTTPMethodSpellings() []string {
	spellings := slices.Clone(httpMethods)
	for _, method := range httpMethods {
		spellings = append(spellings, strings.ToLower(method))
	}
	return spellings
}

// CheckHTTPMethod is a method written as HTTPMethodSpellings has it, in
// upper case, as it is sent, or an error naming the spellings taken.
func CheckHTTPMethod(written string) (string, error) {
	if !slices.Contains(HTTPMethodSpellings(), written) {
		return "", fmt.Errorf("want one of %s, in upper case or lower case", strings.Join(httpMethods, ", "))
	}
	return strings.ToUpper(written), nil
}
