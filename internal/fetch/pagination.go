//go:build !select_request_types || request_type_http

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An API that pages its answer gives one request only the first page. With
// request.pagination an http collector fetches the pages after it too and
// merges them into one JSON document: the first page, with the array its
// items are in (request.pagination.items) holding the items of every page in
// order. The decoder, the transform and the limits then read that document as
// the answer, so a collector written for one page reads every page unchanged.
//
// How the next page is found is request.pagination.type:
//
//   - link: the URL of the Link header's rel="next" (RFC 8288), as GitHub's
//     and GitLab's APIs send it.
//   - next_url: the URL that the jq expression request.pagination.next reads
//     from the page, such as .next or .links.next.
//   - cursor: the value request.pagination.next reads from the page, sent as
//     the query parameter request.pagination.param of the first page's URL.
//   - page: the page number in param, from start (1), one more each page.
//   - offset: the offset in param, from start (0), moved on by the number of
//     items the page held, or by size.
//
// The pages end at the first that names no next one: no rel="next", a next
// expression that gives null, false or "", or, for page and offset, a page
// whose items array is empty or missing; or at max_pages. A trip that stops
// at max_pages while another page is to come is no failure: its answer is
// the pages it fetched, and the stop is counted and logged by the exporter
// (Pages.MoreLeft).
//
// Every page is a request of the collector's own, made by fetch as the first
// is: the same method, body, headers, credentials, TLS settings, retries and
// redirect rules, and the same allowed_targets and denied_targets checked on
// it. A next URL is the target's to name, and is sent the target's
// credentials, so one that leaves the first page's origin, its scheme, host
// and port, is refused, unless it names the virtual host the request's Host
// header asks for, when it is fetched from the first page's host with that
// Host (onVirtualHost); so is one that names a page fetched already, which
// would go round in a circle. The probe's timeout covers every page, and the
// response limit holds each page, the pages together, and the merged
// document.

// validatePagination checks request.pagination at load and fills in its
// defaults: its type, in lower case, max_pages, and start for page and
// offset. The expressions are compiled, so a mistake in one is refused now
// rather than by every scrape.
func validatePagination(c *model.Collector) error {
	p := c.Request.Pagination
	if p == nil {
		return nil
	}
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	switch p.Type {
	case "":
		return fmt.Errorf("collector %q request.pagination.type is required: one of %s", c.Name, strings.Join(model.PaginationTypes, ", "))
	case model.PaginationLink, model.PaginationNextURL, model.PaginationCursor, model.PaginationPage, model.PaginationOffset:
	default:
		return fmt.Errorf("collector %q request.pagination.type is %q; want one of %s", c.Name, p.Type, strings.Join(model.PaginationTypes, ", "))
	}
	if c.Request.Method == http.MethodHead {
		return fmt.Errorf("collector %q sets request.pagination with method HEAD; the pages are read from their bodies, which the answer to a HEAD request has none of", c.Name)
	}
	if strings.TrimSpace(p.Items) == "" {
		return fmt.Errorf("collector %q request.pagination.items is required: the jq path of the array each page holds its items in, such as .items, or . for a page that is an array", c.Name)
	}
	if _, err := itemsPath(p.Items, nil); err != nil {
		return fmt.Errorf("collector %q request.pagination.items %w", c.Name, err)
	}
	usesNext := p.Type == model.PaginationNextURL || p.Type == model.PaginationCursor
	switch {
	case usesNext && strings.TrimSpace(p.Next) == "":
		return fmt.Errorf("collector %q request.pagination.next is required with type %s: the jq expression of the next page's %s, such as .next", c.Name, p.Type, map[bool]string{true: "URL", false: "cursor"}[p.Type == model.PaginationNextURL])
	case !usesNext && p.Next != "":
		return fmt.Errorf("collector %q sets request.pagination.next, which applies to type next_url and cursor; type %s finds the next page %s", c.Name, p.Type, nextPageBy(p.Type))
	}
	if usesNext {
		if _, err := expr.CompileJQ(p.Next); err != nil {
			return fmt.Errorf("collector %q request.pagination.next %q does not compile: %w", c.Name, p.Next, err)
		}
	}
	usesParam := p.Type == model.PaginationCursor || p.Type == model.PaginationPage || p.Type == model.PaginationOffset
	switch {
	case usesParam && strings.TrimSpace(p.Param) == "":
		return fmt.Errorf("collector %q request.pagination.param is required with type %s: the query parameter the %s is sent in", c.Name, p.Type, map[string]string{model.PaginationCursor: "cursor", model.PaginationPage: "page number", model.PaginationOffset: "offset"}[p.Type])
	case !usesParam && p.Param != "":
		return fmt.Errorf("collector %q sets request.pagination.param, which applies to type cursor, page and offset; type %s finds the next page %s", c.Name, p.Type, nextPageBy(p.Type))
	}
	if usesParam {
		for _, r := range p.Param {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("collector %q request.pagination.param %q has the control character %q in it; a parameter's name is sent as written, escaped", c.Name, p.Param, r)
			}
		}
		for name := range c.Request.Query {
			if strings.EqualFold(name, p.Param) {
				return fmt.Errorf("collector %q sets request.query %q, the parameter request.pagination.param sets on every page; leave it out of request.query, or set request.pagination.start for the first page's", c.Name, name)
			}
		}
	}
	countsPages := p.Type == model.PaginationPage || p.Type == model.PaginationOffset
	switch {
	case p.Start != nil && !countsPages:
		return fmt.Errorf("collector %q sets request.pagination.start, which applies to type page and offset", c.Name)
	case p.Start != nil && *p.Start < 0:
		return fmt.Errorf("collector %q request.pagination.start must not be negative", c.Name)
	case p.Start == nil && p.Type == model.PaginationPage:
		p.Start = new(int)
		*p.Start = 1
	case p.Start == nil && p.Type == model.PaginationOffset:
		p.Start = new(int)
	}
	switch {
	case p.Size != 0 && p.Type != model.PaginationOffset:
		return fmt.Errorf("collector %q sets request.pagination.size, which applies to type offset", c.Name)
	case p.Size < 0:
		return fmt.Errorf("collector %q request.pagination.size must be at least 1, or left out to move the offset on by the items each page holds", c.Name)
	}
	if p.MaxPages == 0 {
		p.MaxPages = model.DefaultMaxPages
	}
	if p.MaxPages < 1 || p.MaxPages > model.MaxMaxPages {
		return fmt.Errorf("collector %q request.pagination.max_pages must be from 1 to %d", c.Name, model.MaxMaxPages)
	}
	return nil
}

// nextPageBy says how a pagination of type finds the next page, for the
// message of a key that does not apply to it.
func nextPageBy(typ string) string {
	return map[string]string{
		model.PaginationLink:    `by the Link header's rel="next"`,
		model.PaginationNextURL: "by the URL request.pagination.next reads",
		model.PaginationCursor:  "by the cursor request.pagination.next reads",
		model.PaginationPage:    "by counting pages in request.pagination.param",
		model.PaginationOffset:  "by counting items in request.pagination.param",
	}[typ]
}

// itemsPath is the path, in jq's terms, of the items array that the
// expression items names in the document page: the keys and indexes that
// lead to it. Checked against null at load, it tells an expression that is a
// path, such as .items or .data[0].results, from one that is not, such as
// .items | length, which names no place to put the merged items in.
func itemsPath(items string, page any) ([]any, error) {
	// The expression goes on a line of its own, so a comment at its end
	// does not swallow the closing parenthesis.
	code, err := expr.CompileJQ("path(" + items + "\n)")
	if err != nil {
		return nil, fmt.Errorf("%q does not compile: %w", items, err)
	}
	iterator := code.Run(page, page, nil, nil)
	var found []any
	count := 0
	for {
		value, ok := iterator.Next()
		if !ok {
			break
		}
		if err, ok := value.(error); ok {
			return nil, fmt.Errorf("%q is not a path to the array of a page's items, such as .items, .data.results, or . for a page that is an array: %w", items, err)
		}
		count++
		found, _ = value.([]any)
	}
	if count != 1 {
		return nil, fmt.Errorf("%q names %d places; it must name one, the array of a page's items, such as .items, .data.results, or . for a page that is an array", items, count)
	}
	for _, step := range found {
		switch step.(type) {
		case string, int:
		default:
			return nil, fmt.Errorf("%q is not a path of keys and indexes to the array of a page's items, such as .items, .data.results, or . for a page that is an array", items)
		}
	}
	return found, nil
}

// getPath is the value at path in v, nil when there is none.
func getPath(v any, path []any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, _ := v.(map[string]any)
			v = object[key]
		case int:
			list, _ := v.([]any)
			if key < 0 {
				key += len(list)
			}
			if key < 0 || key >= len(list) {
				return nil
			}
			v = list[key]
		}
	}
	return v
}

// setPath puts value at path in v and returns what v becomes: v itself,
// changed in place, or value for the empty path. A key missing on the way is
// made, as jq's setpath makes it; an index past the end of an array, which
// getPath found nothing at, leaves v as it is.
func setPath(v any, path []any, value any) any {
	if len(path) == 0 {
		return value
	}
	switch key := path[0].(type) {
	case string:
		object, ok := v.(map[string]any)
		if !ok {
			if v != nil {
				return v
			}
			object = map[string]any{}
		}
		object[key] = setPath(object[key], path[1:], value)
		return object
	case int:
		list, _ := v.([]any)
		if key < 0 {
			key += len(list)
		}
		if key >= 0 && key < len(list) {
			list[key] = setPath(list[key], path[1:], value)
		}
	}
	return v
}

// fetchHTTP is the http type's fetch: one request, or the pages of
// request.pagination.
func fetchHTTP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	if c.Request.Pagination == nil {
		return fetch(ctx, target, c, overrides, forwarded)
	}
	return fetchPages(ctx, target, c, overrides, forwarded)
}

// fetchPages fetches the pages of request.pagination and merges them.
func fetchPages(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	p := c.Request.Pagination
	start := time.Now()
	// The probe's timeout covers every page, so it is applied once, here,
	// and not again by each page's fetch.
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
		overrides.Timeout = 0
	}
	first, err := resolveRequestURL(target, c, overrides)
	if err != nil {
		return nil, err
	}
	pageURL := first
	if p.Start != nil {
		pageURL = withQueryParam(first, p.Param, strconv.Itoa(*p.Start))
	}
	limit := responseLimit(c)
	read := &PagesRead{}
	seen := map[string]int{}
	offset := 0
	if p.Start != nil {
		offset = *p.Start
	}
	var firstResponse *HTTPResponse
	var document any
	var path []any
	var items []any
	total := int64(0)
	for n := 1; ; n++ {
		seen[pageURL.String()] = n
		pageOverrides := overrides
		if n > 1 || p.Start != nil {
			pageOverrides.pageURL = pageURL
		}
		response, err := fetch(withPage(ctx, n), target, c, pageOverrides, forwarded)
		if err != nil {
			if n == 1 {
				return nil, err
			}
			return nil, fmt.Errorf("page %d of request.pagination, %s: %w", n, RedactURL(pageURL, MaskCredentialQueryValues), err)
		}
		read.Pages = append(read.Pages, PageRead{URL: pageURL.String(), Status: response.StatusCode, Bytes: len(response.Body)})
		response.Pages = read
		if n == 1 {
			firstResponse = response
		}
		// A page answered with a status the collector does not accept is
		// the trip's answer, which fails as such: the first page's as it
		// would without pagination, a later one's saying which page it was.
		if !AcceptedStatus(c, overrides, response.StatusCode) {
			response.Duration = time.Since(start)
			return response, nil
		}
		total += int64(len(response.Body))
		if total > limit {
			return nil, model.MarkError(fmt.Errorf("the pages of request.pagination come to more than the response limit %d by page %d", limit, n), model.ErrLimitExceeded)
		}
		page, err := decodePage(response.Body)
		if err != nil {
			if n == 1 {
				// The decoder says what is wrong with the first page, as
				// it would without pagination, under on_decode_error.
				firstResponse.Duration = time.Since(start)
				return firstResponse, nil
			}
			return nil, fmt.Errorf("page %d of request.pagination, %s, is not JSON: %w", n, RedactURL(pageURL, MaskCredentialQueryValues), err)
		}
		pagePath, err := itemsPath(p.Items, page)
		if err != nil {
			return nil, fmt.Errorf("page %d of request.pagination: request.pagination.items %w", n, err)
		}
		var pageItems []any
		switch found := getPath(page, pagePath).(type) {
		case nil:
		case []any:
			pageItems = found
		default:
			return nil, fmt.Errorf("page %d of request.pagination: request.pagination.items %q is %s, not an array", n, p.Items, jsonKind(found))
		}
		if n == 1 {
			document, path = page, pagePath
		}
		items = append(items, pageItems...)
		next, err := nextPage(c, response, page, pageURL, len(pageItems), &offset)
		if err != nil {
			return nil, fmt.Errorf("page %d of request.pagination: %w", n, err)
		}
		if next == nil {
			break
		}
		if n == p.MaxPages {
			read.MoreLeft = true
			break
		}
		if !sameOrigin(first, next) {
			onHost, err := onVirtualHost(c, overrides, forwarded, first, next)
			if err != nil {
				return nil, err
			}
			if onHost == nil {
				return nil, fmt.Errorf("page %d of request.pagination names the next page %s, which is not on the target's origin %s; a next page is sent the target's credentials, so it must keep the target's scheme, host and port, or name the virtual host the request's Host header asks for", n, RedactURL(next, MaskCredentialQueryValues), originOf(first))
			}
			next = onHost
		}
		if earlier, ok := seen[next.String()]; ok {
			return nil, fmt.Errorf("page %d of request.pagination names the next page %s, which page %d was; the pages would go round in a circle", n, RedactURL(next, MaskCredentialQueryValues), earlier)
		}
		pageURL = next
	}
	firstResponse.Duration = time.Since(start)
	read.Merged = true
	if len(read.Pages) == 1 {
		// One page is the answer as it came, byte for byte.
		return firstResponse, nil
	}
	if items == nil {
		items = []any{}
	}
	merged, err := encodePage(setPath(document, path, items))
	if err != nil {
		return nil, fmt.Errorf("merging the pages of request.pagination: %w", err)
	}
	if int64(len(merged)) > limit {
		return nil, model.MarkError(model.Errorf("the merged pages of request.pagination come to %d bytes, which exceeds the response limit %d", model.Size(len(merged)), limit), model.ErrLimitExceeded)
	}
	firstResponse.Body = merged
	// The body is the merged document, not what the first page's
	// Content-Length and Content-Encoding described.
	firstResponse.Headers.Del("Content-Length")
	return firstResponse, nil
}

// onVirtualHost is next, a page after the first that is not on the first's
// origin, sent to the first's host instead, when it names the virtual host
// the request asks for with its Host override (hostOverride): the same
// scheme as the first, and the Host's host and port, the scheme's when the
// Host names none. It is nil when there is no Host override or next is
// elsewhere. A target reached by its address with Host: api.example builds
// its links from that Host, so https://10.0.0.5 with Host: api.example is
// answered with Link: <https://api.example/items?page=2>; that page is
// fetched from https://10.0.0.5/items?page=2 with the same Host, as the
// first was. Looking api.example up instead could reach another server,
// which the policy has not checked and which would be sent the target's
// credentials; at the target's address the page stays on the host the
// policy let through, at the virtual host the configuration chose.
func onVirtualHost(c *model.Collector, overrides RequestOverrides, forwarded http.Header, first, next *url.URL) (*url.URL, error) {
	headers, err := renderedValues(c.Request.Headers, "header", "request.headers.", overrides.Params)
	if err != nil {
		return nil, err
	}
	host := hostOverride(headers, forwarded)
	if host == "" || !sameOrigin(&url.URL{Scheme: first.Scheme, Host: host}, next) {
		return nil, nil
	}
	onHost := *next
	onHost.Host = first.Host
	return &onHost, nil
}

// nextPage is the URL of the page after page, the answer to a request for
// pageURL that held count items, or nil when there is none. offset is the
// offset of an offset pagination, moved on here.
func nextPage(c *model.Collector, response *HTTPResponse, page any, pageURL *url.URL, count int, offset *int) (*url.URL, error) {
	p := c.Request.Pagination
	switch p.Type {
	case model.PaginationLink:
		link := nextLink(response.Headers)
		if link == "" {
			return nil, nil
		}
		return resolvePageURL(pageURL, link, `the Link header's rel="next"`)
	case model.PaginationNextURL:
		value, err := nextValue(p.Next, response, page)
		if err != nil || value == "" {
			return nil, err
		}
		return resolvePageURL(pageURL, value, "request.pagination.next")
	case model.PaginationCursor:
		value, err := nextValue(p.Next, response, page)
		if err != nil || value == "" {
			return nil, err
		}
		return withQueryParam(pageURL, p.Param, value), nil
	case model.PaginationPage, model.PaginationOffset:
		if count == 0 {
			return nil, nil
		}
		step := 1
		if p.Type == model.PaginationOffset {
			step = count
			if p.Size > 0 {
				step = p.Size
			}
		}
		*offset += step
		return withQueryParam(pageURL, p.Param, strconv.Itoa(*offset)), nil
	}
	return nil, nil
}

// nextValue is what the expression next gives for page: its text, or ""
// for null, false and "", which end the pagination. A number is its text, as
// a cursor written as one is meant.
func nextValue(next string, response *HTTPResponse, page any) (string, error) {
	code, err := expr.CompileJQ(next)
	if err != nil {
		return "", err
	}
	iterator := code.Run(page, page, response.Status(), jqHeaders(response.Headers))
	var first any
	count := 0
	for {
		value, ok := iterator.Next()
		if !ok {
			break
		}
		if err, ok := value.(error); ok {
			return "", fmt.Errorf("request.pagination.next %q failed: %w", next, err)
		}
		if count == 0 {
			first = value
		}
		count++
	}
	if count > 1 {
		return "", fmt.Errorf("request.pagination.next %q gave %d values; it must give one", next, count)
	}
	switch v := first.(type) {
	case nil:
		return "", nil
	case bool:
		if !v {
			return "", nil
		}
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.Itoa(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case *big.Int:
		return v.String(), nil
	}
	return "", fmt.Errorf("request.pagination.next %q gave %s; it must give text, a number, or null, false or \"\" when there is no next page", next, jsonKind(first))
}

// resolvePageURL is the URL a page names, resolved against the URL of the
// page that names it, as a link is (RFC 3986).
func resolvePageURL(pageURL *url.URL, raw, from string) (*url.URL, error) {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL: %w", from, RedactURLErrors(err))
	}
	next := pageURL.ResolveReference(ref)
	next.Fragment, next.RawFragment = "", ""
	return next, nil
}

// withQueryParam is u with the query parameter name set to value: the pairs
// of u's query that name it, in any case, are dropped (withoutQueryKeys),
// and it goes last.
func withQueryParam(u *url.URL, name, value string) *url.URL {
	own := url.Values{name: {value}}
	next := *u
	next.RawQuery = joinQuery(withoutQueryKeys(u.RawQuery, own), own.Encode())
	return &next
}

// decodePage reads a page as JSON, a number as its text (json.Number), which
// gojq reads as the number and which is written back as it came.
func decodePage(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var page any
	if err := decoder.Decode(&page); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the page goes on after its JSON value")
	}
	return page, nil
}

// encodePage writes the merged document as JSON, with <, > and & as they
// are, for the debug report to show them so.
func encodePage(document any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// nextLink is the target of the Link header's link whose rel names next
// (RFC 8288), or "" when there is none. Each header line may hold several
// links, separated by commas; a link is a URI in angle brackets and its
// parameters, each after a semicolon, a value bare or quoted, and rel is a
// list of relation types separated by spaces, compared without regard to
// case.
func nextLink(h http.Header) string {
	for _, value := range h.Values("Link") {
		for _, link := range parseLinks(value) {
			for _, rel := range strings.Fields(link.rel) {
				if strings.EqualFold(rel, "next") {
					return link.target
				}
			}
		}
	}
	return ""
}

// link is one link of a Link header: its target and its rel.
type link struct{ target, rel string }

// parseLinks reads the links of one Link header line. What is not a link is
// skipped to the next comma outside quotes, so one malformed link does not
// hide the others.
func parseLinks(value string) []link {
	var links []link
	i := 0
	skipBlanks := func() {
		for i < len(value) && (value[i] == ' ' || value[i] == '\t') {
			i++
		}
	}
	// skipToComma moves past the next comma outside a quoted string.
	skipToComma := func() {
		quoted := false
		for ; i < len(value); i++ {
			switch {
			case quoted && value[i] == '\\':
				i++
			case value[i] == '"':
				quoted = !quoted
			case !quoted && value[i] == ',':
				i++
				return
			}
		}
	}
	for i < len(value) {
		skipBlanks()
		if i < len(value) && value[i] == ',' {
			i++
			continue
		}
		if i >= len(value) {
			break
		}
		if value[i] != '<' {
			skipToComma()
			continue
		}
		end := strings.IndexByte(value[i:], '>')
		if end < 0 {
			break
		}
		l := link{target: strings.TrimSpace(value[i+1 : i+end])}
		i += end + 1
		relSeen := false
		for {
			skipBlanks()
			if i >= len(value) || value[i] == ',' {
				break
			}
			if value[i] != ';' {
				skipToComma()
				break
			}
			i++
			skipBlanks()
			nameStart := i
			for i < len(value) && value[i] != '=' && value[i] != ';' && value[i] != ',' && value[i] != ' ' && value[i] != '\t' {
				i++
			}
			name := value[nameStart:i]
			skipBlanks()
			paramValue := ""
			if i < len(value) && value[i] == '=' {
				i++
				skipBlanks()
				if i < len(value) && value[i] == '"' {
					var b strings.Builder
					for i++; i < len(value) && value[i] != '"'; i++ {
						if value[i] == '\\' && i+1 < len(value) {
							i++
						}
						b.WriteByte(value[i])
					}
					i++
					paramValue = b.String()
				} else {
					valueStart := i
					for i < len(value) && value[i] != ';' && value[i] != ',' && value[i] != ' ' && value[i] != '\t' {
						i++
					}
					paramValue = value[valueStart:i]
				}
			}
			// Only the first rel counts (RFC 8288, section 3.3).
			if strings.EqualFold(name, "rel") && !relSeen {
				l.rel, relSeen = paramValue, true
			}
		}
		links = append(links, l)
	}
	return links
}

// withPage marks the requests made with ctx as asking for page n. It lives
// here, behind http's constraint, since only pagination calls it.
func withPage(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, pageKey{}, n)
}
