package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// namePattern is what a collector's and a label's name may look like.
var namePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate checks a configuration and fills in its defaults. It reports
// every mistake it finds, one per line, in the order of the collectors and
// their rules (model.Problems), so a configuration with several mistakes can
// be fixed in one pass. Within one collector, the checks that later checks
// build on (the request, limits, formats and error policies) stop that
// collector at the first mistake. Its metric rules are then each checked in
// full.
func Validate(c *model.Config) error {
	return validate(c, loadOptions{})
}

// validate is Validate with options. namedFilesUnread (WithoutNamedFiles)
// leaves unopened the files the configuration names: the TLS files of
// otlp.tls, the files of web.basic_auth, and the descriptor files of grpc
// collectors. The method and messages of those collectors are then not
// checked against the files. Every other check is made. exporterInstance
// (WithExporterInstanceName) is the value otlp.exporter_instance takes when
// it is left out.
func validate(c *model.Config, options loadOptions) error {
	namedFilesUnread := options.namedFilesUnread
	if len(c.Collectors) == 0 {
		if len(c.CollectorFiles) > 0 {
			return errors.New("no collectors: neither collectors nor the files collector_files matched define any")
		}
		return errors.New("collectors must not be empty")
	}
	var errs []error
	seen := map[string]bool{}
	for i := range c.Collectors {
		x := &c.Collectors[i]
		// A problem with a collector that a collector file defines names
		// that file, which is the one to edit.
		file := collectorFileOf(c, x.Name)
		if err := checkCollectorName(x); err != nil {
			errs = append(errs, inCollectorFile(file, err))
		}
		if seen[x.Name] {
			errs = append(errs, fmt.Errorf("duplicate collector %q", x.Name))
			continue
		}
		seen[x.Name] = true
		if namedFilesUnread {
			x.DescriptorFilesUnread = true
		}
		warned := len(c.Warnings)
		errs = append(errs, inCollectorFile(file, withDefaultsNoted(validateCollector(c, x), x)))
		for j := warned; j < len(c.Warnings); j++ {
			c.Warnings[j] = defaultsNoted(c.Warnings[j], x)
		}
	}
	errs = append(errs, fetch.ValidateHostRateLimits(c.RateLimits), validateWebAuthSettings(c, namedFilesUnread), validateOTLP(&c.OTLP, options))
	errs = append(errs, validateSecretStores(c))
	if err := model.JoinProblems(errs...); err != nil {
		return err
	}
	// Every expression compiled once, and each stays compiled as long as the
	// configuration is in force, however many it has.
	expr.ReserveExpressions(expressionCount(c))
	return nil
}

// checkCollectorName refuses a collector whose name is not one a probe can
// ask for, and Prometheus label values and metric names can carry.
func checkCollectorName(x *model.Collector) error {
	if !namePattern.MatchString(x.Name) {
		return fmt.Errorf("collector %q has invalid name", x.Name)
	}
	return nil
}

// expressionCount is how many expressions the configuration's rules and
// transforms hold, which bounds how many compiled programs it needs.
func expressionCount(c *model.Config) int {
	n := 0
	for i := range c.Collectors {
		x := &c.Collectors[i]
		n += len(x.Transform.Include) + len(x.Transform.Exclude)
		for _, rule := range x.Metrics {
			n += 2 + len(rule.Labels)
		}
	}
	return n
}

// validateCollector checks one collector and fills in its defaults, in the
// order its settings are read: the request, the limits, the cache and the
// names, the response and its transform, the error policies, the metric
// rules, and the derived series.
func validateCollector(c *model.Config, x *model.Collector) error {
	if err := fetch.ValidateRequest(x); err != nil {
		return err
	}
	// The label value placeholders are read before checkPlaceholdersAreFilled,
	// which refuses a placeholder that nothing fills and needs to know which
	// values are filled. They are kept with the collector for its probes.
	labels, err := fetch.ParseLabelParams(x, func(index int) string { return transform.RuleName(&x.Metrics[index], index) })
	if err != nil {
		return err
	}
	x.LabelParams = labels
	x.ExpressionParams = fetch.ParseExpressionParams(x)
	x.TransformClock = fetch.TransformClockUseOf(x)
	// Its requests are timed only when a rule or script reads the timings.
	x.TimingsUnread = !transform.ReadsTimings(x)
	if err := checkPlaceholdersAreFilled(x); err != nil {
		return err
	}
	if err := checkLimitValues(x); err != nil {
		return err
	}
	applyLimitDefaults(&x.Limits)
	if err := validateCache(x); err != nil {
		return err
	}
	if err := transform.ValidateNameEscaping(x); err != nil {
		return err
	}
	if x.MaxConcurrentProbes < 0 {
		return fmt.Errorf("collector %q max_concurrent_probes must not be negative", x.Name)
	}
	if err := validateFailureBackoff(x); err != nil {
		return err
	}
	for _, id := range x.Lint.Ignore {
		if !slices.Contains(model.LintRules, id) {
			return fmt.Errorf("collector %q lint.ignore has unknown rule %q; want one of %s", x.Name, id, strings.Join(model.LintRules, ", "))
		}
	}
	decoderUnset := strings.TrimSpace(x.Decoder.Type) == ""
	if err := normalizeFormats(x); err != nil {
		return err
	}
	// When the decoder is unset and the transform does not imply one, the
	// decoder is chosen per response. That works, but a response that changes
	// its Content-Type, or a file that changes its extension, silently changes
	// how it is read. So the operator is told once on every start and reload.
	if decoderUnset && x.Decoder.Type == "auto" && !requestTypeOf(x).Rows {
		c.Warnings = append(c.Warnings, undecidedDecoderWarning(x))
	}
	// Retries of a POST or PATCH need retry.non_idempotent, since sending one
	// again may repeat what it did; without it they are not made, which the
	// operator who set attempts is told.
	if method := fetch.ConfiguredMethod(x); x.Request.Retry.Attempts > 0 && !x.Request.Retry.NonIdempotent && !fetch.IdempotentMethod(method) {
		c.Warnings = append(c.Warnings, fmt.Sprintf("collector %q sets request.retry.attempts, but its method %s is not idempotent, so a failed request is not retried; set request.retry.non_idempotent to retry it anyway", x.Name, strings.ToUpper(method)))
	}
	for _, policy := range []struct {
		key   string
		value *string
	}{
		{"on_fetch_error", &x.ErrorHandling.OnFetchError},
		{"on_decode_error", &x.ErrorHandling.OnDecodeError},
		{"on_transform_error", &x.ErrorHandling.OnTransformError},
	} {
		if *policy.value == "" {
			*policy.value = model.ErrorPolicyFail
		}
		if err := checkErrorPolicy(x.Name, "error_handling."+policy.key, *policy.value); err != nil {
			return err
		}
	}
	if err := transform.ValidateMetricsPrefix(x); err != nil {
		return err
	}
	if err := checkProbeSeriesNames(x); err != nil {
		return err
	}
	for _, lib := range append(x.Transform.Libraries, x.Transform.RequiredLibs...) {
		if err := checkPythonLibrary(x.Name, lib); err != nil {
			return err
		}
	}
	// The rules and the transform settings are checked apart from each
	// other, and all their mistakes reported; the metric families are
	// checked only when the rules they are made of are sound.
	err = model.JoinProblems(validateMetricRules(c, x), checkCSVColumns(x), transform.CheckTransformSettings(x))
	if err != nil {
		return err
	}
	if err := checkMetricFamilies(x); err != nil {
		return err
	}
	// The derived entries read the rules' names and types, and are checked
	// once those are sound.
	return transform.CheckDerived(x)
}

// checkLimitValues refuses a limit, or request.max_response_bytes, that is
// no limit: negative, or under the least it may be. A defaults block's are
// checked with it too (checkDefaults).
func checkLimitValues(x *model.Collector) error {
	if x.Limits.MaxResponseBytes < 0 {
		return fmt.Errorf("collector %q limits.max_response_bytes must not be negative", x.Name)
	}
	if err := checkLimits(x); err != nil {
		return err
	}
	if err := checkLeastLimits(x); err != nil {
		return err
	}
	if m := x.Limits.MaxScriptMemory; m < 0 || (m > 0 && m < minScriptMemory) {
		return fmt.Errorf("collector %q limits.max_script_memory must be 0, for no limit, or at least 32MiB, since it bounds the Python interpreter and its libraries too; got %d bytes", x.Name, m)
	}
	if m := x.Limits.MaxExpressionMemory; m > 0 && m < minExpressionMemory {
		return fmt.Errorf("collector %q limits.max_expression_memory is %d bytes, and it is at least 1MiB, since it is held against the memory of the whole exporter; leave it out, or 0, for the default, 256MiB", x.Name, m)
	}
	return nil
}

// checkCSVColumns requires a csv transform that reads rows without a header
// row to name its columns by number, from 1. With response.csv.header false
// there is no name to find a column by, and a rule naming one would find
// nothing in any row.
func checkCSVColumns(x *model.Collector) error {
	if x.Transform.Type != "csv" || x.Response.CSV.Header == nil || *x.Response.CSV.Header {
		return nil
	}
	var errs []error
	check := func(what, column string) {
		if n, err := strconv.Atoi(column); err != nil || n < 1 || strconv.Itoa(n) != column {
			errs = append(errs, fmt.Errorf("collector %q %s reads column %q, but response.csv.header is false, so columns are named by number, from 1; write the column's number, such as \"2\"", x.Name, what, column))
		}
	}
	for i := range x.Metrics {
		rule := &x.Metrics[i]
		check(transform.RuleName(rule, i), rule.Expression)
		for _, label := range rule.Labels {
			if !label.Static() {
				check(fmt.Sprintf("%s label %q", transform.RuleName(rule, i), label.Name), label.Expression)
			}
		}
	}
	return model.JoinProblems(errs...)
}

// applyLimitDefaults gives every unset limit its default.
// limits.max_response_bytes is not filled in: when it is unset,
// request.max_response_bytes alone decides, and 10 MiB applies when neither
// is set (fetch.responseLimit).
// minScriptMemory is the smallest limits.max_script_memory allowed. Less is
// not enough for an interpreter with its libraries to start.
const minScriptMemory = 32 << 20

// minExpressionMemory is the least limits.max_expression_memory: the limit
// is held against the growth of the whole exporter's memory, which other
// scrapes move by more than less than this.
const minExpressionMemory = 1 << 20

func applyLimitDefaults(l *model.Limits) {
	if l.MaxMetrics <= 0 {
		l.MaxMetrics = 10000
	}
	if l.MaxLabelsPerMetric <= 0 {
		l.MaxLabelsPerMetric = 20
	}
	if l.MaxLabelValueLength <= 0 {
		l.MaxLabelValueLength = 500
	}
	if l.MaxMetricNameLength <= 0 {
		l.MaxMetricNameLength = 200
	}
	if l.MaxLabelNameLength <= 0 {
		l.MaxLabelNameLength = 200
	}
	if l.MaxHelpLength <= 0 {
		l.MaxHelpLength = 2000
	}
	if l.ScriptTimeout <= 0 {
		l.ScriptTimeout = model.Duration(100 * time.Millisecond)
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = 1 << 20
	}
	if l.MaxCacheEntries <= 0 {
		l.MaxCacheEntries = 1000
	}
	if l.MaxExpressionMemory <= 0 {
		l.MaxExpressionMemory = 256 << 20
	}
}

// requestTypeOf returns the registration of a collector's request type. It
// says what the configuration requires of the collector beyond its request
// block: its decoder and transform. A collector whose type is not built has
// an empty one; validating its request refuses that collector first.
func requestTypeOf(x *model.Collector) *fetch.RequestType {
	if rt := fetch.RequestTypes[x.Request.Type]; rt != nil {
		return rt
	}
	return &fetch.RequestType{}
}

// normalizeFormats refuses a missing or unknown transform and an unknown
// decoder, and infers the decoder a transform implies. Each is taken as
// written: lower case, without surrounding blanks, the only spelling the
// schemas accept.
func normalizeFormats(x *model.Collector) error {
	if err := decode.CheckCharset(x.Response.Charset); err != nil {
		return fmt.Errorf("collector %q response.charset: %w", x.Name, err)
	}
	switch {
	case x.Transform.Type == "":
		return fmt.Errorf("collector %q has no transform.type; it is required: %s", x.Name, strings.Join(model.TransformTypes, ", "))
	case !slices.Contains(model.TransformTypes, x.Transform.Type):
		return fmt.Errorf("collector %q has unknown transform %q; want one of %s, in lower case", x.Name, x.Transform.Type, strings.Join(model.TransformTypes, ", "))
	}
	if x.Decoder.Type == "" {
		x.Decoder.Type = "auto"
	}
	if !slices.Contains(model.DecoderTypes, x.Decoder.Type) {
		return fmt.Errorf("collector %q has unknown decoder %q; want one of %s, in lower case", x.Name, x.Decoder.Type, strings.Join(model.DecoderTypes, ", "))
	}
	rt := requestTypeOf(x)
	if x.Decoder.Type == "auto" && rt.Decoder != "" {
		// A graphite render API answers JSON a jq rule could read as it
		// is, but the graphite decoder is what a graphite collector is
		// for; decoder json reads the answer as it came.
		x.Decoder.Type = rt.Decoder
	}
	if rt.JSONAnswer {
		// A grpc call, a dns lookup, a tlscert handshake, a kubernetes
		// listing, a redis scrape and an ldap search are answered as JSON,
		// whatever else their decoder could be.
		switch x.Decoder.Type {
		case "auto", "json":
			x.Decoder.Type = "json"
		default:
			return fmt.Errorf("collector %q decodes with %s, but %s %s collector's answer is JSON; leave decoder.type out, or set json", x.Name, x.Decoder.Type, fetch.TypeArticle(x.Request.Type), x.Request.Type)
		}
		switch x.Transform.Type {
		case "jq", "yq", "python":
		default:
			return fmt.Errorf("collector %q transforms with %s, which cannot read the JSON %s %s collector is answered with; use jq, yq or python", x.Name, x.Transform.Type, fetch.TypeArticle(x.Request.Type), x.Request.Type)
		}
	}
	if rt.JSONDocument != nil {
		if document := rt.JSONDocument(x); document != "" {
			switch x.Decoder.Type {
			case "auto", "json":
				x.Decoder.Type = "json"
			default:
				return fmt.Errorf("collector %q decodes with %s, but it is answered with %s; leave decoder.type out, or set json", x.Name, x.Decoder.Type, document)
			}
			switch x.Transform.Type {
			case "jq", "yq", "python":
			default:
				return fmt.Errorf("collector %q transforms with %s, which cannot read %s; use jq, yq or python", x.Name, x.Transform.Type, document)
			}
		}
	}
	if err := checkChainAnswer(x); err != nil {
		return err
	}
	if fetch.CollectorPolicy(x).Pagination != nil {
		// The pages are read as JSON to find the next one and their items,
		// and merged into one JSON document, whatever their Content-Type.
		switch x.Decoder.Type {
		case "auto", "json":
			x.Decoder.Type = "json"
		default:
			return fmt.Errorf("collector %q decodes with %s, but request.pagination reads its pages as JSON and merges them into one JSON document; leave decoder.type out, or set json", x.Name, x.Decoder.Type)
		}
	}
	if rt.Rows {
		if err := checkSQLFormats(x); err != nil {
			return err
		}
	}
	if x.Decoder.Type == "auto" && !rt.Rows {
		switch x.Transform.Type {
		case "regex":
			x.Decoder.Type = "text"
		case "csv":
			x.Decoder.Type = "csv"
		case "css":
			x.Decoder.Type = "html"
		case "prometheus":
			x.Decoder.Type = "prometheus"
		}
	}
	if x.Transform.Type == "python" && strings.TrimSpace(x.Transform.Script) == "" {
		return fmt.Errorf("collector %q Python transform requires a script", x.Name)
	}
	if err := checkGraphiteResponse(x); err != nil {
		return err
	}
	if err := checkBinaryResponse(x); err != nil {
		return err
	}
	if err := checkProtobufResponse(x); err != nil {
		return err
	}
	// A decoder the transform can never read would fail every scrape.
	if err := transform.CheckDecoder(x); err != nil {
		return err
	}
	return checkSettingsApply(x)
}

// checkSQLFormats checks what reads a sql collector's rows. The driver reads
// them value by value, so no decoder and no response setting applies to
// them. Only the transforms that read rows by column or a list of objects
// can map them.
func checkSQLFormats(x *model.Collector) error {
	if x.Decoder.Type != "auto" {
		return fmt.Errorf("collector %q decodes with %s, but a sql collector's rows are read by its driver, with no body to decode; leave decoder.type out", x.Name, x.Decoder.Type)
	}
	switch x.Transform.Type {
	case "csv", "jq", "yq", "python":
	default:
		return fmt.Errorf("collector %q transforms with %s, which cannot read the rows of a statement; use csv, which reads a column by name, one series a row, or jq, yq or python, which read the rows as a list of objects", x.Name, x.Transform.Type)
	}
	if x.Transform.Type == "csv" && fetch.SQLNamedResults(x) {
		return fmt.Errorf("collector %q transforms with csv, which reads the rows of one statement by column, and runs request.statements, whose document is an object of each one's rows by its name; use jq, yq or python, which read .<name>, or a collector of each statement", x.Name)
	}
	if r := x.Response; r.Charset != "" || r.CSV != (model.CSVConfig{}) || len(r.Namespaces) > 0 || r.Graphite != (model.GraphiteConfig{}) || r.Binary.Set() || r.YAML != (model.YAMLConfig{}) {
		return fmt.Errorf("collector %q sets response settings, which apply to the body of a response, and a sql collector's rows have none; leave the response block out", x.Name)
	}
	return nil
}

// checkProtobufResponse checks response.protobuf and the protobuf decoder,
// which reads a body as the message it names (fetch/protobufdecoder.go).
// The decoder needs a message and the files that describe it: a descriptor
// set or .proto files, not both. The block is for the decoder, chosen by
// decoder.type. A build without the grpc request type has neither. The files
// are read now, so a message that is not in them fails the configuration,
// unless it was loaded without them (model.Collector.DescriptorFilesUnread).
func checkProtobufResponse(x *model.Collector) error {
	p := x.Response.Protobuf
	switch {
	case p.Set() && x.Decoder.Type != "protobuf":
		return fmt.Errorf("collector %q sets response.protobuf, the message of the protobuf decoder, but its decoder is %s; set decoder.type to protobuf", x.Name, cmp.Or(x.Decoder.Type, "chosen by the transform or the answer"))
	case x.Decoder.Type != "protobuf":
		return nil
	case !fetch.ProtobufDecoder:
		return fmt.Errorf("collector %q decodes with protobuf: %w", x.Name, fetch.CheckProtobufDecoder(x))
	case strings.TrimSpace(p.Message) == "":
		return fmt.Errorf("collector %q decodes with protobuf and has no response.protobuf.message; name the message the body is, by its full name, such as metrics.v1.Report", x.Name)
	case p.ProtosetFile != "" && (len(p.ProtoFiles) > 0 || len(p.ProtoImportPaths) > 0):
		return fmt.Errorf("collector %q sets response.protobuf.protoset_file and proto_files or proto_import_paths; the message is read from a descriptor set or from .proto files, so keep one", x.Name)
	case p.ProtosetFile == "" && len(p.ProtoFiles) == 0:
		return fmt.Errorf("collector %q decodes with protobuf and names no file that describes %s; set response.protobuf.proto_files to the .proto files, or protoset_file to a descriptor set that protoc --descriptor_set_out --include_imports or buf build -o writes", x.Name, p.Message)
	}
	for i, file := range p.ProtoFiles {
		if strings.TrimSpace(file) == "" {
			return fmt.Errorf("collector %q response.protobuf.proto_files[%d] is empty", x.Name, i)
		}
	}
	for i, dir := range p.ProtoImportPaths {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("collector %q response.protobuf.proto_import_paths[%d] is empty", x.Name, i)
		}
	}
	switch x.Transform.Type {
	case "jq", "yq", "python":
	default:
		return fmt.Errorf("collector %q decodes with protobuf, whose document is JSON-like, and its transform is %s; use jq, yq or python", x.Name, cmp.Or(x.Transform.Type, "unset"))
	}
	if x.DescriptorFilesUnread {
		return nil
	}
	if err := fetch.CheckProtobufDecoder(x); err != nil {
		return fmt.Errorf("collector %q response.protobuf: %w", x.Name, err)
	}
	return nil
}

// checkBinaryResponse checks response.binary and the binary decoder, which
// reads a body by that layout alone (decode/binary.go). The decoder needs a
// layout, and a layout is for the decoder, chosen by decoder.type, since no
// response is recognized as binary by its content. Its document is
// JSON-like, which jq, yq and python read. Each field has a type. A field
// that appears in the document has a name no other field has; a skip, which
// appears in none, has no name. A string and a skip have a length in bytes,
// a cstring may have one (the most bytes it reads), and a number has none.
// count, for a list of numbers, is at most model.MaxBinaryCount. Nothing is
// negative.
func checkBinaryResponse(x *model.Collector) error {
	b := x.Response.Binary
	switch {
	case b.Set() && x.Decoder.Type != "binary":
		return fmt.Errorf("collector %q sets response.binary, the layout of the binary decoder, but its decoder is %s; set decoder.type to binary", x.Name, cmp.Or(x.Decoder.Type, "chosen by the transform or the answer"))
	case x.Decoder.Type != "binary":
		return nil
	case len(b.Fields) == 0:
		return fmt.Errorf("collector %q decodes with binary and response.binary.fields is empty; list the fields the answer holds, in order, such as - {name: registers, type: u16, count: 2}", x.Name)
	case len(b.Fields) > model.MaxBinaryFields:
		return fmt.Errorf("collector %q response.binary has %d fields, more than the %d it may have", x.Name, len(b.Fields), model.MaxBinaryFields)
	case b.ByteOrder != "" && !slices.Contains(model.BinaryByteOrders, b.ByteOrder):
		return fmt.Errorf("collector %q response.binary.byte_order is %q; want big or little, in lower case", x.Name, b.ByteOrder)
	}
	switch x.Transform.Type {
	case "jq", "yq", "python":
	default:
		return fmt.Errorf("collector %q decodes a binary answer, which a %s transform cannot read; use jq, yq or python, whose rules read the fields by name, as in path: .registers[0]", x.Name, x.Transform.Type)
	}
	names := map[string]int{}
	for i, f := range b.Fields {
		at := fmt.Sprintf("collector %q response.binary.fields[%d]", x.Name, i)
		if !slices.Contains(model.BinaryTypes, f.Type) {
			return fmt.Errorf("%s type is %q; want one of %s, in lower case", at, f.Type, strings.Join(model.BinaryTypes, ", "))
		}
		switch {
		case f.Type == "skip" && f.Name != "":
			return fmt.Errorf("%s is a skip and has the name %q; a skip passes bytes over and is in no document, so leave its name out", at, f.Name)
		case f.Type != "skip" && f.Name == "":
			return fmt.Errorf("%s has no name; name the field the rules read it by", at)
		case f.Offset != nil && *f.Offset < 0:
			return fmt.Errorf("%s offset is %d; it is the byte the field starts at, from 0", at, *f.Offset)
		case f.ByteOrder != "" && !slices.Contains(model.BinaryByteOrders, f.ByteOrder):
			return fmt.Errorf("%s byte_order is %q; want big or little, in lower case", at, f.ByteOrder)
		case f.Count < 0 || f.Count > model.MaxBinaryCount:
			return fmt.Errorf("%s count is %d; it is how many values the field holds, from 1 to %d, or left out for one", at, f.Count, model.MaxBinaryCount)
		case f.Length < 0:
			return fmt.Errorf("%s length is %d; it is how many bytes the field is", at, f.Length)
		}
		textual := f.Type == "string" || f.Type == "cstring" || f.Type == "skip"
		switch {
		case (f.Type == "string" || f.Type == "skip") && f.Length == 0:
			return fmt.Errorf("%s is a %s and has no length; write how many bytes it is", at, f.Type)
		case !textual && f.Length != 0:
			return fmt.Errorf("%s is a %s, whose length is its type's, and sets length; leave length out, or write count for several of them", at, f.Type)
		case textual && f.Count != 0:
			return fmt.Errorf("%s is a %s and sets count; count is for a list of numbers", at, f.Type)
		}
		if f.Name != "" {
			if j, ok := names[f.Name]; ok {
				return fmt.Errorf("%s has the name %q of fields[%d]; each field is a member of the document, so name them apart", at, f.Name, j)
			}
			names[f.Name] = i
		}
	}
	return nil
}

// checkGraphiteResponse checks response.graphite; value defaults to last. It
// applies to the graphite decoder, which a per-response decoder choice may
// turn out to be. A collector with another decoder refuses the block, since
// it would be ignored. The graphite decoder's document is JSON-like, so only
// the transforms that read one can map it.
func checkGraphiteResponse(x *model.Collector) error {
	g := &x.Response.Graphite
	set := *g != model.GraphiteConfig{}
	if set && x.Decoder.Type != "graphite" && x.Decoder.Type != "auto" {
		return fmt.Errorf("collector %q sets response.graphite, which applies to the graphite decoder, but its decoder is %s", x.Name, x.Decoder.Type)
	}
	if g.Value != "" && !slices.Contains(model.GraphiteValues, g.Value) {
		return fmt.Errorf("collector %q response.graphite.value is %q; want one of %s, in lower case", x.Name, g.Value, strings.Join(model.GraphiteValues, ", "))
	}
	if g.MaxAge < 0 {
		return fmt.Errorf("collector %q response.graphite.max_age must not be negative", x.Name)
	}
	if g.InvalidLines != "" && !slices.Contains(model.GraphiteInvalidLines, g.InvalidLines) {
		return fmt.Errorf("collector %q response.graphite.invalid_lines is %q; want fail or skip, in lower case", x.Name, g.InvalidLines)
	}
	if x.Decoder.Type == "graphite" {
		switch x.Transform.Type {
		case "jq", "yq", "python":
		default:
			return fmt.Errorf("collector %q decodes Graphite series, which a %s transform cannot read; use jq, yq or python, whose rules read the series document, as in items: .series[]", x.Name, x.Transform.Type)
		}
	}
	return nil
}

// validateMetricRules checks each metric rule and fills in its defaults. It
// reports the mistakes of every rule. A rule's own settings stop that rule at
// the first mistake, and then all its expressions are checked
// (CheckMetricRuleAt). A python rule that passes both is told about
// everything it sets that such a rule does not take (checkPythonRule). A
// prometheus rule that passes them all must still say which metrics it is
// about (checkPrometheusRuleSelects). The rules that pass all of that are
// then compared with each other. No two may be the same rule, and no
// prometheus rule's expression may match the name of a metric another rule
// passes on with the same labels (checkRulesDiffer,
// checkNoPatternRepeatsAName).
func validateMetricRules(c *model.Config, x *model.Collector) error {
	// The maps of the files the rules name are part of the rules from here
	// on (valuemapfiles.go).
	if err := readValueMapFiles(x); err != nil {
		return err
	}
	var errs []error
	sound := make([]bool, len(x.Metrics))
	for i := range x.Metrics {
		err := validateMetricRule(x, i)
		if err == nil {
			err = checkPrometheusRuleSelects(x, i)
		}
		sound[i] = err == nil
		errs = append(errs, err)
	}
	errs = append(errs, transform.CheckLabelValueMapsAgree(x))
	errs = append(errs, checkRulesDiffer(x, sound)...)
	return model.JoinProblems(errs...)
}

// checkRulesDiffer refuses two rules of a collector that are the same rule:
// alike in everything that decides which series a rule makes and from what.
// Each then makes every series the other makes, with one name and one set of
// labels, and every scrape fails on the first of them with `validation
// failed: duplicate metric series "up"`. A configuration mistake, such as a
// rule pasted twice, is then reported as a scrape failure for as long as the
// exporter runs.
//
// Two rules are the same rule when they are alike in:
//
//   - name, expression and items, each as written, "" being the key left
//     out;
//   - their labels: the same names, each with the same value or the same
//     expression (ruleLabels);
//   - value_map and time_format, which say which texts the rule reads a
//     value from.
//
// The last two are compared because rules that differ in them need not make
// the same series. A rule reads no value from a text its value_map does not
// hold or its time_format does not fit. So rules alike except for those can
// each read what the other cannot (a field that is "up" or a number, a time
// written one way or another), and a scrape gets the one series.
//
// Everything else a rule sets changes the value or the help text of its
// series, or what happens when it has none, but never which series it is:
// scale and time_zone, description, required and error_mode, and a label's
// required and truncate. Two rules that differ only in those are the same
// rule, as are two that differ in nothing. A label's value_map is looked up
// by the rule's name, so rules of one name share it
// (CheckLabelValueMapsAgree), and a name has one type (checkMetricFamilies).
// Neither can tell two such rules apart.
//
// A python rule makes no series; the script does. Two alike python rules
// name one of the script's series twice, which changes nothing, so they load.
//
// The comparison is of what is written. Two expressions that mean the same
// but are written differently count as two rules: they load, and fail the
// scrape as before. One pair of rules that select the same thing in other
// words is decided by the configuration alone, and is refused after the
// copies: a prometheus rule without an expression, which matches the metric
// of its name, and one whose expression matches that name
// (checkNoPatternRepeatsAName).
//
// Only rules that passed the per-rule checks (sound) are compared. So a rule
// those checks refuse gets the same message as before, and is told about its
// copy once it gets that far. Each rule that repeats an earlier one is
// reported against the first of its kind. Both are named by their places
// among the collector's rules, counted from 1 as CheckLabelValueMapsAgree
// counts them.
func checkRulesDiffer(x *model.Collector, sound []bool) []error {
	if x.Transform.Type == "python" {
		return nil
	}
	// Rules alike in the four texts are few, and only those have their
	// labels and value_maps compared.
	type texts struct{ name, expression, items, labelsFrom, timeFormat string }
	var errs []error
	alike := map[texts][]int{}
	// The rules that repeat no earlier one, in their order.
	var distinct []int
	for i := range x.Metrics {
		if !sound[i] {
			continue
		}
		r := &x.Metrics[i]
		key := texts{r.Name, r.Expression, r.Items, r.LabelsFrom, r.TimeFormat}
		first := slices.IndexFunc(alike[key], func(earlier int) bool {
			other := &x.Metrics[earlier]
			return maps.Equal(r.ValueMap, other.ValueMap) && alikeInLabels(x, r, other)
		})
		if first < 0 {
			alike[key] = append(alike[key], i)
			distinct = append(distinct, i)
			continue
		}
		of := fmt.Sprintf("of metric %q", r.Name)
		if r.Name == "" {
			of = fmt.Sprintf("of the metrics that match %q", r.Expression)
		}
		errs = append(errs, fmt.Errorf("collector %q metrics rule %d and rule %d are the same rule %s: alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each", x.Name, alike[key][first]+1, i+1, of))
	}
	return append(errs, checkNoPatternRepeatsAName(x, distinct)...)
}

// checkNoPatternRepeatsAName refuses a pair of rules of a prometheus
// collector where one rule has no expression and passes on the metric of its
// name, the other has an expression that matches that same name, and both
// give the metric's series one name and the same labels:
//
//	metrics:
//	  - name: up
//	  - expression: '^up'
//
// Each of them then makes every series of the metric. A scrape that has a
// series twice fails with `validation failed: duplicate metric series "up"`,
// on every scrape of a target that has the metric. For a target without it,
// the rule of the name makes nothing and, unless it is not required, reports
// the metric missing. So the rule of the name, which is only about that
// metric, makes a series in no scrape that passes. The configuration alone
// shows this: the name is written, and whether a regular expression matches
// a known text is decided here as the transform decides it for each series,
// with the expression compiled the same way and matched anywhere in the name
// (transform.applyPrometheusTransform).
//
// The rules give the series one name when the rule with the expression has
// no name, and so keeps the metric's own, or has that same name, under which
// it exports what it matches. With another name it exports the metric under
// that name, a different family, and the two rules are not compared. Their
// labels are the same when the rules are alike in them the way two identical
// rules are (alikeInLabels). No prometheus rule has a value_map or a
// time_format, which would tell such rules apart (transform.CheckMetricRule).
//
// Nothing else the rule of the name sets changes this. With a type the other
// rule lacks, the scrape fails on a metric of inconsistent types before it
// reaches the duplicate. Where its scale or type cannot apply to the metric
// (a histogram or a summary), or a label it requires is missing, the rule
// fails on the series instead of making it, and still makes none.
//
// Other settings of the rule with the expression can change what a scrape
// does, but with one exception they do not change the verdict. That rule can
// also fail on a series of the metric (canFailOnASeries): its type or scale
// cannot apply to a histogram or a summary, which keeps its own type and
// takes no scale; a type of histogram or summary applies to nothing else;
// and a series may lack a label the rule requires. Under error_mode log or
// ignore it then carries on without that series, and the scrape passes with
// the series the rule of the name made. So the result of such a pair depends
// on the target: `{name: lat}` beside `{expression: '.*',
// scale: 0.001}` passes where lat is a histogram and fails on a duplicate
// where it is a gauge, and `{name: up}` beside `{expression: '^up$', type:
// gauge}` fails every scrape of a target whose up is a gauge, a counter or
// untyped. Such pairs are refused with the others: a setting that fails on
// some series cannot keep a rule away from a metric, since every series it
// does not fail on is made twice. The error says that, instead of saying that
// every series is made twice, and names what the rule sets.
//
// The exception is a rule with an expression that is only about histograms,
// or only about summaries (keepsToItsType): `{name: up}` beside
// `{expression: '.*', type: histogram, error_mode: ignore}` means the metric
// up and every histogram, and loads. That type applies to no metric of
// another type. So the rule fails on every gauge, counter and untyped metric
// and every metric of the other of the two types, and under error_mode log
// or ignore it carries on without them. That leaves an ordinary metric to the
// rule of its name. The pair makes a series twice only where the target's
// metric of that name is itself of that type, and such a target's scrape
// fails on the duplicate, as it does for two rules whose expressions both
// match a metric. Anything else the rule sets, a scale or a required label,
// comes after its type and keeps it from more series, not fewer. The pair is
// still refused where the rule of the name sets that same type, since it
// then makes the metric only where the other rule makes it too. It is also
// refused where the error_mode is fail, since the rule then fails the scrape
// on the first metric of another type.
//
// Nothing else is refused. Two rules that both have an expression, such as
// '^node_' and '^node_cpu', make a series twice only where the target has a
// metric both match, which no configuration says. The same holds for a rule
// of a name and one that exports another metric under that name, and for
// rules whose labels differ as written but come out the same on a target.
// Those load, and fail the scrape that has such a series, as before.
//
// The rules compared are those that repeat no earlier rule (distinct), in
// their order. A copy is reported as the same rule as the first of its kind
// (checkRulesDiffer) and for nothing else. So no pair is reported twice, and
// removing the copy leaves what is reported for the first. Each pair is
// reported once, at the later of its two rules. Both are named by their
// places among the collector's rules, counted from 1.
func checkNoPatternRepeatsAName(x *model.Collector, distinct []int) []error {
	if x.Transform.Type != "prometheus" {
		return nil
	}
	var errs []error
	for at, later := range distinct {
		for _, earlier := range distinct[:at] {
			named, matching := earlier, later
			if x.Metrics[named].Expression != "" {
				named, matching = later, earlier
			}
			n, m := &x.Metrics[named], &x.Metrics[matching]
			if n.Name == "" || n.Expression != "" || m.Expression == "" || m.Name != "" && m.Name != n.Name || keepsToItsType(n, m) {
				continue
			}
			if pattern, err := expr.CompileRegex(m.Expression); err != nil || !pattern.MatchString(n.Name) || !alikeInLabels(x, n, m) {
				continue
			}
			twice := ", so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series"
			if settings := canFailOnASeries(m); len(settings) > 0 && m.ErrorMode != model.ErrorModeFail {
				// The rule carries on without a series it fails on, so not
				// every series need be made twice; and the two make one of
				// two types where one sets a type the other has not.
				does, list := "does", settings[0]
				if last := len(settings) - 1; last > 0 {
					does, list = "do", strings.Join(settings[:last], ", ")+" and "+settings[last]
				}
				types := ""
				if n.Type != m.Type {
					types = " or, where the two rules give the metric different types, as a metric of inconsistent types"
				}
				twice = fmt.Sprintf("; %s of rule %d %s not keep it from the metric, since every series of the metric that rule %d does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series%s", list, matching+1, does, matching+1, types)
			}
			errs = append(errs, fmt.Errorf("collector %q metrics rule %d and rule %d both pass on metric %q: rule %d passes on the metric of that name, the expression %q of rule %d matches that name, and their labels are alike%s; take one of the two out, write the expression so that it does not match %q, or tell their series apart by a label, as with a static label that has another value in each", x.Name, earlier+1, later+1, n.Name, named+1, m.Expression, matching+1, twice, n.Name))
		}
	}
	return errs
}

// keepsToItsType reports whether a prometheus rule with an expression, with
// its defaults filled in, leaves to a rule of a name the metric that rule
// passes on, by its type. That holds when the type is histogram or summary,
// which applies to a metric of that type and no other
// (transform.applyPrometheusTransform), and the rule carries on without the
// series it fails on, because its error_mode is not fail. The rule of the
// name must also not set that type itself: it would then make the metric
// only where it is of that type, as the other rule does.
func keepsToItsType(named, matching *model.MetricRule) bool {
	ofOneShape := matching.Type == model.HistogramMetricType || matching.Type == model.SummaryMetricType
	return ofOneShape && matching.ErrorMode != model.ErrorModeFail && named.Type != matching.Type
}

// canFailOnASeries names what a prometheus rule sets that can fail on a
// series the rule matches (transform.applyPrometheusTransform), in the words
// an error uses. These are the type and the scale, which cannot apply to
// every metric (a histogram or a summary keeps its own type and takes no
// scale, and nothing else becomes one), and each label the rule requires,
// which is read from the series and may be missing from it. Under error_mode
// log or ignore the rule carries on without such a series.
func canFailOnASeries(r *model.MetricRule) []string {
	var settings []string
	if r.Type != "" {
		settings = append(settings, "the type")
	}
	if r.Scale != nil {
		settings = append(settings, "the scale")
	}
	for _, label := range r.Labels {
		if !label.Required {
			continue
		}
		// A label written twice is named once.
		if setting := fmt.Sprintf("the required label %q", label.Name); !slices.Contains(settings, setting) {
			settings = append(settings, setting)
		}
	}
	return settings
}

// alikeInLabels says whether two rules of a collector give a series the
// same labels as far as the rules say: the same names, each with the same
// value or the same expression (ruleLabels).
func alikeInLabels(x *model.Collector, a, b *model.MetricRule) bool {
	return slices.EqualFunc(ruleLabels(x, a), ruleLabels(x, b), func(a, b model.LabelRule) bool {
		return a.Name == b.Name && a.Value == b.Value && a.Expression == b.Expression
	})
}

// ruleLabels returns a rule's labels as alikeInLabels compares them: in the
// order of their names, since a series gets a label by its name wherever the
// rule writes it. So two rules whose labels are written in another order are
// the same rule. Labels of one name keep their written order, and the last
// of them is the one a series gets.
//
// A prometheus rule's label reads a label of the series the rule passes on,
// and a label written before it may have set that. With labels a, read from
// b, and b, a constant, a is the target's b when a is written first, and the
// constant when b is. Where a label of such a rule reads one the rule sets
// under another name, the order decides the series, and the labels are
// compared in their written order.
func ruleLabels(x *model.Collector, r *model.MetricRule) []model.LabelRule {
	if x.Transform.Type == "prometheus" {
		for _, label := range r.Labels {
			if !label.Static() && label.Expression != label.Name && slices.ContainsFunc(r.Labels, func(other model.LabelRule) bool { return other.Name == label.Expression }) {
				return r.Labels
			}
		}
	}
	labels := slices.Clone(r.Labels)
	slices.SortStableFunc(labels, func(a, b model.LabelRule) int { return strings.Compare(a.Name, b.Name) })
	return labels
}

// checkPrometheusRuleSelects refuses the rule at index of a prometheus
// collector's metrics when it has neither a name nor an expression. Such a
// rule passes on the metrics whose names its expression matches, or, without
// an expression, the metric its name names. With neither, the pattern it was
// matched by came to ^$ (transform.applyPrometheusTransform,
// decode.prometheusKeeps), which is no metric's name. So the rule loaded and
// selected nothing by construction. Being required unless it says otherwise,
// it was reported as missing on every scrape, `metric "" is not in the
// response`: a configuration mistake reported as the target's. With
// required: false nothing was ever said. Whatever else it set (a type, a
// description, labels) applied to no series.
//
// A key written "" is the key left out, so name: "" and expression: "" count
// as neither. A name or an expression of only blanks is refused before this,
// as no metric name and as no pattern (transform.CheckMetricRule).
//
// The rule has no name, so the error says which rule of the collector it is,
// counted from 1 as CheckLabelValueMapsAgree counts them. It runs after every
// check that existed before it, so a rule those checks refuse keeps the
// message it had.
func checkPrometheusRuleSelects(x *model.Collector, index int) error {
	if r := x.Metrics[index]; x.Transform.Type != "prometheus" || r.Name != "" || r.Expression != "" {
		return nil
	}
	return fmt.Errorf("collector %q metrics rule %d has neither a name nor an expression, and a prometheus rule needs one of them to say which of the target's metrics it passes on: an expression, a regular expression matched against a metric's name as the target gives it, anywhere in it, to match metrics by, or a name, to match the metric of that name; with neither it matches no metric, so write one, as in expression: '^node_' or name: up, or expression: '.*' for a rule about every metric, or take the rule out", x.Name, index+1)
}

// validateMetricRule checks the metric rule at index of the collector's
// metrics and fills in its defaults. Its messages name the rule as
// transform.RuleWhere does: by its metric name or, when it has no name, by
// its place among the collector's rules.
func validateMetricRule(x *model.Collector, index int) error {
	r, where := &x.Metrics[index], transform.RuleWhere(x, index)
	// Whether the rule wrote a type and an error_mode is asked before their
	// defaults are filled in, after which a key left out reads as one that
	// was written (checkPythonRule).
	wroteType, wroteErrorMode := r.Type != "", r.ErrorMode != ""
	if r.ErrorMode == "" {
		r.ErrorMode = model.ErrorModeLog
	}
	if err := checkErrorPolicy(x.Name, transform.RuleName(r, index)+" error_mode", r.ErrorMode); err != nil {
		return err
	}
	// A prometheus transform's rule without a type keeps the type of
	// the series it passes through: a counter stays a counter, a
	// histogram a histogram. Every other rule makes its own samples,
	// gauges unless it says otherwise.
	if r.Type == "" && x.Transform.Type != "prometheus" {
		r.Type = model.GaugeMetricType
	}
	// A histogram block asks for a histogram of the values the rule reads,
	// and is checked before the type, which it is what allows.
	if err := checkHistogramRule(x, r, where); err != nil {
		return err
	}
	switch r.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	case "":
	case model.HistogramMetricType, model.SummaryMetricType:
		// Only a series that already is a histogram or a summary has buckets
		// or quantiles to expose. A rule reading one number would expose a
		// histogram with a single plain sample, which no parser accepts. A rule
		// with a histogram block counts its values in buckets of its own.
		if r.Histogram != nil {
			break
		}
		if x.Transform.Type != "prometheus" {
			return fmt.Errorf("%s has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped%s", where, r.Type, r.Type, x.Transform.Type, histogramBlockAdvice(r.Type, x.Transform.Type))
		}
	default:
		return fmt.Errorf("%s has invalid type %q", where, r.Type)
	}
	// The rule has no name to be named by, so where says which rule of the
	// collector it is.
	if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
		return fmt.Errorf("%s has no name", where)
	}
	if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
		return fmt.Errorf("%s has no expression", where)
	}
	names := make(map[string]bool, len(r.Labels))
	for j, label := range r.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("%s has a label without a name", where)
		}
		// Two labels of one name would be one label on the series, whose
		// value the later of them had and the earlier lost unsaid.
		if names[label.Name] {
			return fmt.Errorf("%s has two labels named %q; a series has one label of a name, so remove one of them or give it another name", where, label.Name)
		}
		names[label.Name] = true
		// A label name must be valid for a scrape under the collector's
		// name_escaping, unless transform.remove_labels or rename_labels
		// takes it off before the scrape checks it
		// (transform.TakesLabelNameBeforeRenames).
		if !transform.TakesLabelNameBeforeRenames(x, label.Name) {
			return fmt.Errorf("%s has invalid label name %q%s", where, label.Name, transform.EscapingAdvice(label.Name))
		}
		if err := transform.CheckLabelNameBeforeRenames(x, label.Name); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		// An expression of only blanks is not the key left out (only "" is),
		// and it cannot read a label either. The label is then not static
		// (model.LabelRule.Static), so a value beside it was never exported,
		// while a csv rule read the column of that name and a prometheus rule
		// the source label of that name.
		if label.Expression != "" && strings.TrimSpace(label.Expression) == "" {
			return fmt.Errorf("%s label %q expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant", where, label.Name, label.Expression)
		}
		hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
		// A value that holds a placeholder (fetch.ParseLabelParams) is not
		// always present like a constant: it is what the probe gives. So it
		// cannot be required either, and the error says why. A python rule's
		// label takes no value at all, placeholder or not, and is left to the
		// cases that say so.
		templated := hasValue && fetch.TemplatedRuleLabel(x, index, j)
		switch {
		case hasValue && hasExpression:
			return fmt.Errorf("%s label %q sets both value and expression; set value for a static label, or expression to read it from the response", where, label.Name)
		case !hasValue && !hasExpression:
			return fmt.Errorf("%s label %q needs a value, for a static label, or an expression, to read it from the response", where, label.Name)
		case templated && label.Required && x.Transform.Type != "python":
			return fmt.Errorf("%s label %q cannot be required: its value has a {{param_...}} placeholder, so it comes from the probe, and required is for a label an expression reads from the response. A probe that leaves out a parameter whose placeholder has no default is answered 400, and a placeholder with an empty default, {{param_<name>:}}, leaves the label off because the configuration says so; remove required", where, label.Name)
		case hasValue && label.Required && !templated:
			return fmt.Errorf("%s label %q has a static value, so it cannot be required; its value is always there", where, label.Name)
		case label.Required && x.Transform.Type == "python":
			return fmt.Errorf("%s label %q cannot be required: a python transform's labels come from its script, not from label expressions", where, label.Name)
		}
	}
	if err := checkPythonRuleLabels(x, r, where); err != nil {
		return err
	}
	if err := transform.CheckMetricRuleAt(x, index); err != nil {
		return err
	}
	return checkPythonRule(x, r, where, wroteType, wroteErrorMode)
}

// histogramBlockAdvice ends the refusal of a rule's type histogram, which
// its transform cannot give, by pointing to the histogram block, which makes
// a histogram of the values the rule reads. It does so where the transform
// takes the block: every transform but python, whose script makes its
// series itself.
func histogramBlockAdvice(t model.MetricType, transform string) string {
	if t != model.HistogramMetricType || transform == "python" {
		return ""
	}
	return ", or add a histogram block with the upper bounds of its buckets, as in histogram: {buckets: [0.1, 0.5, 1]}, to count the values the rule reads in a histogram"
}

// checkPythonRuleLabels refuses a constant on a label of a python rule. The
// script makes the series and sets their labels itself. A rule makes no
// series, and the only thing its label does is name a label of the script's
// series to cut with truncate: true (transform.truncateLabels). A value there
// used to be accepted and did nothing: the series did not get the constant,
// and nothing was said. A constant for every series belongs in
// transform.labels. where names the rule.
func checkPythonRuleLabels(x *model.Collector, r *model.MetricRule, where string) error {
	if x.Transform.Type != "python" {
		return nil
	}
	for _, label := range r.Labels {
		if label.Value != "" {
			return fmt.Errorf("%s label %q sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels", where, label.Name)
		}
	}
	return nil
}

// checkPythonRule refuses settings on a python collector's rule that such a
// rule does not take. The script makes the series, gives each its type and
// help text, and fails the scrape itself; a rule makes no series. A rule
// names one of the script's series. Its name is that series' name: a debug
// probe's report lists the rule by it when the script made no such series
// (writeTransform in the exporter), and its labels are found by it. Each of
// its labels names a label of that series to cut with truncate: true
// (transform.truncateLabels). Everything else a rule can set was accepted
// there and did nothing, with nothing said:
//
//   - type and description, which the script gives: a rule saying counter
//     left the series the gauge the script made;
//   - required and error_mode, which are about a value the rule fails to
//     read: {name: up, error_mode: fail} did not fail a scrape whose script
//     made no up;
//   - a rule without a name, which names no series, so its labels cut
//     nothing whatever truncate said;
//   - a label without truncate: true, which cuts nothing: its expression,
//     which the label check requires, is not read, so an author who wrote one
//     for a label of the series got no such label.
//
// Each is refused with a note on where the script sets it instead. All of a
// rule's problems are reported together, the rule named by where (by its
// place among the collector's rules when the name is what it lacks).
// wroteType and wroteErrorMode say the rule wrote those keys, which their
// defaults have since filled in. A text key written "" is the key left out,
// here as everywhere. required counts as written when it is present, true or
// false, since the rule tells it apart from the key left out.
//
// It runs last, after every earlier check, so a rule those checks refuse
// keeps the message it had. The one thing left that a python rule may set
// and nothing reads is its expression, as the documentation says.
func checkPythonRule(x *model.Collector, r *model.MetricRule, where string, wroteType, wroteErrorMode bool) error {
	if x.Transform.Type != "python" {
		return nil
	}
	var errs []error
	if r.Name == "" {
		errs = append(errs, fmt.Errorf("%s has no name, which a python rule needs: the rule makes no series and only names one of the script's, whose labels it cuts with truncate: true and which a debug probe's report lists when the script made none; write the name the script gives the series, as in metric(\"up\", ...), or take the rule out", where))
	}
	if wroteType {
		errs = append(errs, fmt.Errorf("%s sets type, which a python rule does not take: the script gives each of its series its type, with metric(..., type=\"counter\"), and a series is a gauge when it gives none; say the type in the script and leave type out of the rule", where))
	}
	if r.Description != "" {
		errs = append(errs, fmt.Errorf("%s sets description, which a python rule does not take: the script gives each of its series its help text, with metric(..., help=\"...\"), and a series has none when it gives none; say the help in the script and leave description out of the rule", where))
	}
	if r.Required != nil {
		errs = append(errs, fmt.Errorf("%s sets required, which a python rule does not take: the rule makes no series, so it has no value to miss; a script that cannot do without something fails the scrape itself, with fail(\"...\"), so say it in the script and leave required out of the rule", where))
	}
	if wroteErrorMode {
		errs = append(errs, fmt.Errorf("%s sets error_mode, which a python rule does not take: the rule makes no series, so it has no failure to handle; a script fails the scrape itself, with fail(\"...\"), and error_handling.on_transform_error says what the collector does then, so leave error_mode out of the rule", where))
	}
	for _, label := range r.Labels {
		if !label.Truncate {
			errs = append(errs, fmt.Errorf("%s label %q does not set truncate: true, which is all a python rule's label does: it names a label of the script's series to cut to limits.max_label_value_length, and its expression is not read; the script sets the labels of its series itself, with metric(..., labels={...}), so set truncate: true on the label or take it out", where, label.Name))
		}
	}
	return model.JoinProblems(errs...)
}

// validateWebAuthSettings checks the exporter's own basic authentication,
// which cannot share the Authorization header with a collector forwarding it.
// namedFilesUnread leaves its files unread (validate).
func validateWebAuthSettings(c *model.Config, namedFilesUnread bool) error {
	if c.Web.BasicAuth == nil || !c.Web.BasicAuth.Enabled {
		return nil
	}
	if err := validateWebAuth(c.Web.BasicAuth, namedFilesUnread); err != nil {
		return err
	}
	for i := range c.Collectors {
		collector := &c.Collectors[i]
		if collector.Request.ForwardAuthorization {
			return fmt.Errorf("web.basic_auth cannot be enabled with collector %q request.forward_authorization", collector.Name)
		}
	}
	return nil
}

// otlpServiceNameAttribute is the resource attribute otlp.service_name, and a
// static target's otlp.service_name, are exported as.
const otlpServiceNameAttribute = "service.name"

// validateOTLP checks the OTLP export settings and fills in their defaults.
// options.namedFilesUnread leaves the TLS files unread (validate).
func validateOTLP(o *model.OTLPConfig, options loadOptions) error {
	if !o.Enabled {
		return nil
	}
	namedFilesUnread := options.namedFilesUnread
	if strings.TrimSpace(o.Endpoint) == "" {
		return errors.New("otlp.endpoint is required when OTLP is enabled")
	}
	u, ok := otlpEndpointURL(o.Endpoint)
	if !ok {
		return errors.New("otlp.endpoint must be an http or https URL")
	}
	// What the export sends and connects with is held to what a collector's
	// request is: a header Go would refuse, or half a client certificate,
	// would otherwise load and then fail every export.
	if err := fetch.CheckHeaders(o.Headers); err != nil {
		return fmt.Errorf("otlp.headers %w", err)
	}
	checkTLS := fetch.CheckClientTLS
	if namedFilesUnread {
		checkTLS = fetch.CheckClientTLSKeys
	}
	if err := checkTLS(o.TLS); err != nil {
		return fmt.Errorf("otlp.tls %w", err)
	}
	switch {
	case o.Timeout < 0:
		return fmt.Errorf("otlp.timeout must not be negative; got %s", time.Duration(o.Timeout))
	case o.Timeout == 0:
		o.Timeout = model.Duration(5 * time.Second)
	}
	// An export may retry for up to an interval, and each is a request of
	// every pending point: under a second it would be a busy loop.
	switch {
	case o.Interval == 0:
		o.Interval = model.Duration(30 * time.Second)
	case o.Interval < model.Duration(time.Second):
		return fmt.Errorf("otlp.interval %s is under the least, 1s; leave it out for the default, 30s", time.Duration(o.Interval))
	}
	// service_name is the resource's service.name; set among the attributes
	// too, the resource would carry the key twice.
	if _, twice := o.ResourceAttributes[otlpServiceNameAttribute]; twice {
		return fmt.Errorf("otlp.resource_attributes sets %s, which otlp.service_name sets; write the name as service_name", otlpServiceNameAttribute)
	}
	if o.ServiceName == "" {
		o.ServiceName = "prometheus-universal-exporter"
	}
	if o.MaxPendingPoints < 0 {
		return errors.New("otlp.max_pending_points must not be negative")
	}
	if o.MaxPendingPoints == 0 {
		o.MaxPendingPoints = model.DefaultOTLPMaxPendingPoints
	}
	switch {
	case o.BatchMaxSize < 0:
		return fmt.Errorf("otlp.batch_max_size must not be negative; got %d, and leaving it out, or 0, is the default, %d", o.BatchMaxSize, model.DefaultOTLPBatchMaxSize)
	case o.BatchMaxSize == 0:
		o.BatchMaxSize = model.DefaultOTLPBatchMaxSize
	}
	// Under the least, a request would hold little more than its resource
	// and the names of its metrics.
	switch {
	case o.BatchMaxBytes == 0:
		o.BatchMaxBytes = model.DefaultOTLPBatchMaxBytes
	case o.BatchMaxBytes < model.MinOTLPBatchMaxBytes:
		return fmt.Errorf("otlp.batch_max_bytes is %d bytes, under the least, 64KiB; set at least 64KiB, or leave it out, or 0, for the default, 4MiB", o.BatchMaxBytes)
	}
	switch {
	case o.UnreadyAfterFailures < 0:
		return errors.New("otlp.unready_after_failures must not be negative")
	case o.UnreadyAfterFailures > model.MaxOTLPUnreadyAfterFailures:
		return fmt.Errorf("otlp.unready_after_failures is %d, over the most, %d", o.UnreadyAfterFailures, model.MaxOTLPUnreadyAfterFailures)
	}
	switch o.Compression {
	case "":
		o.Compression = model.OTLPCompressionGzip
	case model.OTLPCompressionGzip, model.OTLPCompressionNone:
	default:
		return fmt.Errorf("otlp.compression must be %s or %s, not %q", model.OTLPCompressionGzip, model.OTLPCompressionNone, o.Compression)
	}
	switch o.MetricNames {
	case "":
		o.MetricNames = model.OTLPMetricNamesPrometheus
	case model.OTLPMetricNamesPrometheus, model.OTLPMetricNamesOTel:
	default:
		return fmt.Errorf("otlp.metric_names must be %s or %s, not %q", model.OTLPMetricNamesPrometheus, model.OTLPMetricNamesOTel, o.MetricNames)
	}
	if err := checkOTLPScope(o.Scope); err != nil {
		return err
	}
	if err := checkExporterInstance(&o.ExporterInstance, o.ResourceAttributes, options); err != nil {
		return err
	}
	if err := validateOTLPLogs(&o.Logs, u); err != nil {
		return err
	}
	return validateOTLPTraces(&o.Traces, u)
}

// hostname is os.Hostname; a variable for the tests.
var hostname = os.Hostname

// checkExporterInstance checks otlp.exporter_instance of an enabled otlp
// block and fills in its defaults. The attribute defaults to
// model.DefaultOTLPExporterInstanceAttribute. The value defaults to the name
// WithExporterInstanceName gave, the --allocator.member name in allocator
// mode, or else the host name, read once here at each load. A pod's host name
// is its name, and a replica's host name is what tells it from the others.
// Without a value there is nothing to name the replica with, so the
// configuration is refused instead of leaving the attribute out, which would
// silently merge the replicas' data under one resource. The target allocator
// reads the configuration it serves without the named files, and is not the
// replica whose name the value is, so it leaves the value as written.
//
// The attribute is added to every resource (exporter/otlp.go), so it may not
// be service.name, which otlp.service_name sets, or a key of
// otlp.resource_attributes, written "" or not. The resource would carry the
// key twice, or one of the two would silently win. A static target's
// resource is checked against it with the targets (checkTargetResourceKeys).
func checkExporterInstance(e *model.OTLPExporterInstanceConfig, attributes map[string]string, options loadOptions) error {
	if !e.Enabled {
		return nil
	}
	switch {
	case e.Attribute == "":
		e.Attribute = model.DefaultOTLPExporterInstanceAttribute
	case strings.TrimSpace(e.Attribute) == "":
		return errors.New("otlp.exporter_instance.attribute is blanks alone; name an attribute, or leave it out for the default, " + model.DefaultOTLPExporterInstanceAttribute)
	}
	for _, key := range []struct{ name, value string }{{"attribute", e.Attribute}, {"value", e.Value}} {
		for _, r := range key.value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("otlp.exporter_instance.%s has the control character %q in it", key.name, r)
			}
		}
	}
	if e.Attribute == otlpServiceNameAttribute {
		return fmt.Errorf("otlp.exporter_instance.attribute is %s, which otlp.service_name sets; name another attribute, or leave it out for the default, %s", otlpServiceNameAttribute, model.DefaultOTLPExporterInstanceAttribute)
	}
	if _, twice := attributes[e.Attribute]; twice {
		return fmt.Errorf("otlp.exporter_instance.attribute is %s, which otlp.resource_attributes sets too; leave one of the two out", e.Attribute)
	}
	if e.Value != "" || options.namedFilesUnread {
		return nil
	}
	if options.exporterInstance != "" {
		e.Value = options.exporterInstance
		return nil
	}
	host, err := hostname()
	if err == nil && host == "" {
		err = errors.New("it is empty")
	}
	if err != nil {
		return fmt.Errorf("otlp.exporter_instance.value is left out and the host name, its default, cannot be read (%w); set the value, such as ${POD_NAME} with --config.expand-env", err)
	}
	e.Value = host
	return nil
}

// checkOTLPScope checks otlp.scope when it is written. A scope is identified
// by its name, so the name must not be empty. The name and the version are
// sent as written, and a backend does not show a control character.
func checkOTLPScope(scope *model.OTLPScopeConfig) error {
	if scope == nil {
		return nil
	}
	if scope.Name == "" {
		return fmt.Errorf("otlp.scope.name is required when otlp.scope is written; leave scope out for the default, %s", model.DefaultOTLPScopeName)
	}
	for _, key := range []struct{ name, value string }{{"name", scope.Name}, {"version", scope.Version}} {
		for _, r := range key.value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("otlp.scope.%s has the control character %q in it", key.name, r)
			}
		}
	}
	return nil
}

// validateOTLPLogs checks otlp.logs of an enabled block whose endpoint is
// metrics, and fills in its defaults. The logs go to the same collector as
// the metrics, at its standard path for logs, unless the block names another
// endpoint. An endpoint that does not end in /v1/metrics has no standard
// path to derive one from, so the block must then name one.
func validateOTLPLogs(l *model.OTLPLogsConfig, metrics *url.URL) error {
	if !l.Enabled {
		return nil
	}
	switch {
	case strings.TrimSpace(l.Endpoint) != "":
		if _, ok := otlpEndpointURL(l.Endpoint); !ok {
			return errors.New("otlp.logs.endpoint must be an http or https URL")
		}
	case strings.HasSuffix(metrics.Path, otlpMetricsPath):
		derived := *metrics
		derived.Path = strings.TrimSuffix(metrics.Path, otlpMetricsPath) + otlpLogsPath
		derived.RawPath = ""
		l.Endpoint = derived.String()
	default:
		return fmt.Errorf("otlp.logs.endpoint is required when otlp.endpoint does not end in %s, which would be made %s for the logs", otlpMetricsPath, otlpLogsPath)
	}
	level, ok := model.LogLevel(l.Level)
	switch {
	case l.Level == "":
		l.Level = "info"
	case !ok:
		return fmt.Errorf("otlp.logs.level %q is not a level; use debug, info, warn or error", l.Level)
	default:
		l.Level = strings.ToLower(level.String())
	}
	switch {
	case l.Interval == 0:
		l.Interval = model.Duration(model.DefaultOTLPLogsInterval)
	case l.Interval < model.Duration(time.Second):
		return fmt.Errorf("otlp.logs.interval %s is under the least, 1s; leave it out for the default, %s", time.Duration(l.Interval), model.DefaultOTLPLogsInterval)
	}
	switch {
	case l.MaxPendingRecords < 0:
		return errors.New("otlp.logs.max_pending_records must not be negative")
	case l.MaxPendingRecords == 0:
		l.MaxPendingRecords = model.DefaultOTLPLogsMaxPendingRecords
	}
	return nil
}

// validateOTLPTraces checks otlp.traces of an enabled block whose endpoint
// is metrics, and fills in its defaults. Like the logs, the traces go to the
// same collector as the metrics, at its standard path for traces, unless the
// block names another endpoint. It must name one where otlp.endpoint does
// not end in /v1/metrics.
func validateOTLPTraces(t *model.OTLPTracesConfig, metrics *url.URL) error {
	if !t.Enabled {
		return nil
	}
	switch {
	case strings.TrimSpace(t.Endpoint) != "":
		if _, ok := otlpEndpointURL(t.Endpoint); !ok {
			return errors.New("otlp.traces.endpoint must be an http or https URL")
		}
	case strings.HasSuffix(metrics.Path, otlpMetricsPath):
		derived := *metrics
		derived.Path = strings.TrimSuffix(metrics.Path, otlpMetricsPath) + otlpTracesPath
		derived.RawPath = ""
		t.Endpoint = derived.String()
	default:
		return fmt.Errorf("otlp.traces.endpoint is required when otlp.endpoint does not end in %s, which would be made %s for the traces", otlpMetricsPath, otlpTracesPath)
	}
	// A threshold under a millisecond would export the trace of nearly
	// every probe, which is no longer the slow ones.
	switch {
	case t.Threshold == 0:
		t.Threshold = model.Duration(model.DefaultOTLPTracesThreshold)
	case t.Threshold < model.Duration(model.MinOTLPTracesThreshold):
		return fmt.Errorf("otlp.traces.threshold %s is under the least, 1ms; leave it out for the default, %s", time.Duration(t.Threshold), model.DefaultOTLPTracesThreshold)
	}
	switch {
	case t.Interval == 0:
		t.Interval = model.Duration(model.DefaultOTLPTracesInterval)
	case t.Interval < model.Duration(time.Second):
		return fmt.Errorf("otlp.traces.interval %s is under the least, 1s; leave it out for the default, %s", time.Duration(t.Interval), model.DefaultOTLPTracesInterval)
	}
	// A ratio outside 0 to 1 is no share of the traces; NaN is none either,
	// and fails both comparisons.
	if !(t.SampleRatio >= 0 && t.SampleRatio <= 1) {
		return fmt.Errorf("otlp.traces.sample_ratio %v is not between 0 and 1", t.SampleRatio)
	}
	if t.MaxPerSecond < 0 {
		return errors.New("otlp.traces.max_per_second must not be negative")
	}
	switch {
	case t.MaxPendingSpans < 0:
		return errors.New("otlp.traces.max_pending_spans must not be negative")
	case t.MaxPendingSpans == 0:
		t.MaxPendingSpans = model.DefaultOTLPTracesMaxPendingSpans
	}
	return nil
}

// otlpEndpointURL reads an OTLP/HTTP endpoint, which is an http or https
// URL with a host.
func otlpEndpointURL(endpoint string) (*url.URL, bool) {
	u, err := url.Parse(endpoint)
	return u, err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// The standard paths of OTLP/HTTP's metrics, logs and traces.
const (
	otlpMetricsPath = "/v1/metrics"
	otlpLogsPath    = "/v1/logs"
	otlpTracesPath  = "/v1/traces"
)

// LoadOption adjusts how a configuration document is read. Options are
// variadic so that reading a file without them — which is what every test and
// every default path does — stays the plain call it was.
type LoadOption func(*loadOptions)

type loadOptions struct {
	expandEnv bool
	// flag is the flag that turned expansion on, named when a reference
	// cannot be expanded.
	flag string
	// namedFilesUnread is WithoutNamedFiles.
	namedFilesUnread bool
	// exporterInstance is WithExporterInstanceName.
	exporterInstance string
}

// Each document has its own expansion flag: --config.expand-env for the
// configuration and its collector files, and --static-targets.expand-env for
// the static target file. The static target file holds the addresses and
// credentials of what is scraped. An operator may want those from the
// environment while the configuration is committed as written, or the other
// way round.
const (
	configEnvFlag        = "--config.expand-env"
	staticTargetsEnvFlag = "--static-targets.expand-env"
)

// WithEnvExpansion substitutes ${NAME} references from the process environment
// before the configuration is parsed. It is what --config.expand-env turns on.
func WithEnvExpansion() LoadOption {
	return func(o *loadOptions) { o.expandEnv, o.flag = true, configEnvFlag }
}

// WithStaticTargetsEnvExpansion does for the static target file what
// WithEnvExpansion does for the configuration. It is what
// --static-targets.expand-env turns on.
func WithStaticTargetsEnvExpansion() LoadOption {
	return func(o *loadOptions) { o.expandEnv, o.flag = true, staticTargetsEnvFlag }
}

// WithoutNamedFiles loads the configuration without opening the files it
// names (namedFiles): the TLS files of otlp.tls, the username and password
// files of web.basic_auth, and a grpc collector's protoset_file and
// proto_files with their imports. Every other check is made. A grpc
// collector whose descriptors come from files is not checked against them,
// and neither are the messages of the static targets that use it. The target
// allocator uses this to check the configuration it serves: those files are
// mounted on the replicas, which check them when they load it, and not on
// the allocator. The configuration and its collector files are read as
// always.
func WithoutNamedFiles() LoadOption {
	return func(o *loadOptions) { o.namedFilesUnread = true }
}

// WithExporterInstanceName sets the value otlp.exporter_instance takes when
// it is left out, in place of the host name. It is the --allocator.member
// name of an exporter in allocator mode, the name the allocator knows the
// replica by. "" means the host name.
func WithExporterInstanceName(name string) LoadOption {
	return func(o *loadOptions) { o.exporterInstance = name }
}

// readDocument reads a configuration file, a collector file or a static
// target file. A file it cannot read is a readError, which the watch tries
// again at every tick (Manager.configUnread).
func readDocument(path string, opts []LoadOption) ([]byte, error) {
	b, _, err := readDocumentSource(path, opts)
	return b, err
}

// readDocumentSource is readDocument, which also gives the file as it is
// written, before its references were expanded (recordWritten).
func readDocumentSource(path string, opts []LoadOption) (document, raw []byte, err error) {
	var options loadOptions
	for _, apply := range opts {
		apply(&options)
	}
	raw, err = readFile(path)
	if err != nil {
		return nil, nil, &readError{err: err}
	}
	if !options.expandEnv {
		return raw, raw, nil
	}
	document, err = expandEnvironment(path, raw, options.flag)
	return document, raw, err
}

// readFile reads the file at path for readDocument; tests set it to make a
// file unreadable in a way the watch's stamps do not see, which the user
// running the tests may be able to read whatever its permissions.
var readFile = os.ReadFile

// readError is the error for a file readDocument could not read: missing,
// unreadable because of its own or a directory's permissions, or itself a
// directory. Its text is that of the wrapped error. Such a file may become
// readable without its stamp changing (its owner, an ACL or a directory's
// permissions changed). So while a reload is refused for this error, the
// watch reads the file again at every tick. It does not do so for a reload
// refused for what a file says.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// unreadable reports whether err says a file could not be read (readError),
// alone or among other problems.
func unreadable(err error) bool {
	var re *readError
	return errors.As(err, &re)
}

// Load reads the configuration file at path and the collector files it
// lists, and validates the result.
func Load(path string, opts ...LoadOption) (*model.Config, error) {
	return load(path, opts, nil)
}

// load is Load. decoded, when given, is called with the configuration as its
// files write it, once they could be read and before validation. Validation
// opens the files the configuration names (namedFiles), so a reload that
// stamps them stamps them before they are read.
func load(path string, opts []LoadOption, decoded func(*model.Config)) (*model.Config, error) {
	b, raw, err := readDocumentSource(path, opts)
	if err != nil {
		return nil, err
	}
	if err = refuseNotScalarKeys(b); err != nil {
		return nil, err
	}
	var c model.Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = withValueProblems(withoutExtensionKeys(dec.Decode(&c)), b, &c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("configuration file %s is empty; it must define collectors", path)
		}
		return nil, yamlError(err)
	}
	if err = oneDocument(dec); err != nil {
		return nil, err
	}
	// The configuration's defaults are its own collectors', given before
	// the collector files' join them.
	ownDefaultsErr := applyOwnDefaults(&c, b)
	recordWritten(raw, c.Collectors)
	err = model.JoinProblems(ownDefaultsErr, mergeCollectorFiles(&c, path, opts))
	if decoded != nil {
		decoded(&c)
	}
	var options loadOptions
	for _, apply := range opts {
		apply(&options)
	}
	// The collectors that were read are checked even when a collector file
	// could not be read, so one run reports the mistakes of every file. The
	// exception is a file whose defaults were refused: checked without the
	// values they would have taken, its collectors would be refused for what
	// the defaults block was to give them. A collector file's collectors are
	// left out with it (loadCollectorFile), and the configuration's own here.
	if err != nil {
		if ownDefaultsErr != nil {
			c.Collectors = slices.DeleteFunc(c.Collectors, func(x model.Collector) bool { return c.CollectorSources[x.Name] == path })
		}
		if len(c.Collectors) > 0 {
			err = model.JoinProblems(err, validate(&c, options))
		}
		return nil, err
	}
	if err = validate(&c, options); err != nil {
		return nil, err
	}
	return &c, nil
}

// Manager holds the configuration in force and the static target file,
// and reloads them: when the watch finds a file changed, on SIGHUP and on
// POST /-/reload. A rejected reload leaves the previous configuration in
// force.
type Manager struct {
	// current is the configuration and the static target file in force,
	// one value, so a reload puts both in force at once (inForce).
	current atomic.Pointer[inForce]
	path    string
	logger  *slog.Logger
	// lastMod, lastSize and lastMode are the configuration file's
	// modification time, size and mode when it was last read. A change to any
	// of them counts as a change. So a file replaced by one with an older time
	// (as cp -p and rsync -t leave it) is read too, and so is a file whose
	// permissions changed, which may have made it readable again, or
	// unreadable.
	lastMod  time.Time
	lastSize int64
	lastMode fs.FileMode
	// collectorFiles is the stamp of the collector files the configuration
	// read when last loaded (collectorFilesStamp). watchedFiles are the
	// collector_files entries the stamp was taken from: those of the file
	// last read, even when it was refused, so the watch sees a fix to a
	// collector file it added.
	collectorFiles string
	watchedFiles   []string
	// retryFiles are the files that the last read configuration names and
	// that loading it opens (namedFiles), together with those of the
	// configuration in force. retryStamp is their state just before they were
	// read (filesStamp). They are kept while that configuration is refused, so
	// a change to one of them is a change the watch sees. A reload refused
	// because a certificate was being replaced is then retried once it is in
	// place, although the configuration itself is unchanged. While it is
	// refused, the files its .proto files import are watched too.
	// retryConfigs are the configurations the files were taken from: the one
	// in force, then the one read. retryImports says there are such files.
	// With the watch on, those of the configuration read were stamped before
	// it was validated (loading), and those of the one in force join
	// retryFiles (retryImported). Without the watch, when nothing compares a
	// stamp, those of both join, as they did before the descriptor files in
	// force were watched.
	retryFiles     []string
	retryStamp     string
	retryConfigs   []*model.Config
	retryImports   bool
	targetPath     string
	targetsLastMod time.Time
	// targetsLastSize and targetsLastMode are lastSize and lastMode for the
	// static target file.
	targetsLastSize int64
	targetsLastMode fs.FileMode
	pythonPath      string
	watchInterval   time.Duration
	expandEnv       bool
	// expandStaticTargetsEnv is expandEnv for the static target file.
	expandStaticTargetsEnv bool
	// exporterInstance is the name the configuration was first read with
	// for otlp.exporter_instance (WithExporterInstanceName).
	exporterInstance string
	// targetsRetryFiles and targetsRetryStamp are retryFiles and retryStamp
	// for the static target file. They hold the files opened by checking the
	// last read file against the configuration (targetsNamedFiles) and, with
	// the watch on, the files those import, each as it was before the check
	// read it. They are kept while that file is refused. targetsRetryChecked
	// are the collectors they were taken from (targetsChecked). Without the
	// watch, the imported files join them when the file is refused.
	targetsRetryFiles   []string
	targetsRetryStamp   string
	targetsRetryChecked *model.Config
	// descriptors are the descriptor files of the configuration in force:
	// the request.protoset_file and request.proto_files of its grpc
	// collectors and the files those .proto files import. Their state is the
	// one just before the load that last read the configuration read them
	// (stampDescriptors). Unlike retryFiles, they are kept while the
	// configuration is in force. The configuration is checked against them,
	// so a change to one is a change the watch reloads for. The reload then
	// finds, at the tick and not at the probes that then fail, a descriptor
	// that no longer has the collector's method or takes its message.
	// loading are the descriptor files of the configuration loadConfig last
	// read, stamped before it was validated. They become descriptors when it
	// is put in force. While it is refused they stay watched as they are, so
	// a file that the refused load read, and that changed at any time since,
	// makes the configuration be read again. Both are empty without the
	// watch: nothing would compare the stamps, so none is taken, and no
	// descriptor file is read to learn what it imports. descriptorsStamped
	// says descriptors were taken for the configuration in force, at startup
	// (UseStamp) or when the watch was turned on (SetWatchInterval).
	descriptors, loading stampedFiles
	descriptorsStamped   bool
	// configWaits and targetsWaits say that file's last reload was refused
	// only because the other file, as in force, disagrees with it, so a
	// change to the other file reads it again (apply).
	configWaits, targetsWaits bool
	// configUnread and targetsUnread say that file's last reload was refused
	// because a file it reads could not be read (readError). While so, the
	// watch reads it again at every tick: the file may have become readable
	// without any change its stamps hold, through its owner, an ACL or a
	// directory's permissions (reloadChanged). configRefusal and
	// targetsRefusal are the text by which the last refusal of that file is
	// recognised (model.SameFailureText), "" once the file is in force.
	// configQuiet and targetsQuiet say the current reload reads that file
	// only to retry it, so a refusal with the same text as the last is
	// neither logged nor counted again (rejectConfig).
	configUnread, targetsUnread   bool
	configRefusal, targetsRefusal string
	configQuiet, targetsQuiet     bool
	// reloads records how loading each file has gone, for the self-metrics.
	Reloads *reloadStatus
	// reloadMu serializes reloads, whatever triggers them.
	reloadMu sync.Mutex
	// installed are told of every reload that put something in force
	// (OnInstall), under reloadMu.
	installed []func()
	// prepared are given what every reload is about to put in force
	// (OnPrepare), under reloadMu.
	prepared []func(*model.Config, *model.StaticTargetFile)
	// awaiting says nothing has been put in force yet: the manager of an
	// exporter in allocator mode that started without a bundle
	// (NewAwaitingManager) holds an empty configuration until its first
	// reload puts one in force (allocatormode.go).
	awaiting atomic.Bool
	// absentTargets says a static target file that does not exist is one
	// without targets rather than a file that cannot be read
	// (AllowAbsentTargets).
	absentTargets bool
	// generation is the generation of the allocator's bundle that the files
	// at the manager's paths are. It is "" outside allocator mode or for no
	// bundle. What a reload puts in force carries it (install). It changes
	// with the files, under reloadMu (ReloadReplacing, SetGeneration).
	generation string
	// discovery is the dns_sd sources of the static target file's
	// discovered_targets (staticdiscoveryrun.go).
	discovery staticDiscovery
	// lastReload is what the last reload that put something in force
	// changed (reloadchanges.go), nil until one has.
	lastReload atomic.Pointer[ReloadSummary]
	// fingerprint tells a collector's definitions apart for the summary
	// (SetFingerprint), nil for their YAML encodings.
	fingerprint func(*model.Config, int) string
}

// inForce is the configuration and the static target file in force. The two
// are checked against each other (a target names a collector and fills its
// placeholders), so they are published as one value that is never changed
// once stored. A reader that needs both takes them in one read
// (Manager.InForce). So it never sees the configuration of one reload with
// the targets of another, as it could when each was stored separately.
// targets is nil without a static target file.
type inForce struct {
	config  *model.Config
	targets *model.StaticTargetFile
	// generation is the generation of the allocator's bundle the two were
	// read from, "" for none (Manager.Generation). It is stored with them, so
	// the reported generation in force never names one whose configuration is
	// not, or no longer, in force.
	generation string
}

// DefaultWatchInterval is how often an enabled watch re-stats the configuration
// files. Changes are detected by modification time rather than by filesystem
// events, because Kubernetes republishes a mounted ConfigMap by swapping the
// `..data` symlink atomically: that replaces the inode a file-level event watch
// is attached to, so such a watch stops firing after the first change.
const DefaultWatchInterval = 60 * time.Second

// NewManager returns a manager holding c, which was read from path.
func NewManager(c *model.Config, path string, l *slog.Logger) *Manager {
	m := &Manager{path: path, logger: l, Reloads: newReloadStatus()}
	m.current.Store(&inForce{config: c})
	if c != nil {
		m.Reloads.loaded(reloadFileConfig)
	}
	if c != nil {
		m.watchCollectorFiles(c.CollectorFiles)
	}
	// The file as read at startup is not a change for the first watch tick.
	// Its descriptor files are stamped when the watch is turned on
	// (SetWatchInterval), and not at all without it.
	if st, err := os.Stat(path); err == nil && c != nil {
		m.lastMod, m.lastSize, m.lastMode = st.ModTime(), st.Size(), st.Mode()
	}
	return m
}

// Stamp is what the watch compares files against to detect a change: their
// modification times, sizes and modes, and the collector files a
// configuration lists. It is taken before the files are read, so a change
// written while they were being read is seen by the next watch tick instead
// of becoming part of what was read.
type Stamp struct {
	mod            time.Time
	size           int64
	mode           fs.FileMode
	found          bool
	entries        []string
	collectorFiles string
	// descriptors are the descriptor files of the configuration read with
	// the stamp (LoadStamped), nil when none was.
	descriptors *stampedFiles
}

// TakeStamp stamps the file at path as it is now. For a configuration file,
// listsCollectorFiles, it stamps the collector files it lists too.
func TakeStamp(path string, listsCollectorFiles bool, opts ...LoadOption) Stamp {
	var s Stamp
	if st, err := os.Stat(path); err == nil {
		s.mod, s.size, s.mode, s.found = st.ModTime(), st.Size(), st.Mode(), true
	}
	if listsCollectorFiles {
		if listed, ok := listedCollectorFiles(path, opts); ok {
			s.entries = listed
			s.collectorFiles = collectorFilesStamp(path, listed)
		}
	}
	return s
}

// LoadStamped is Load for the configuration an exporter starts with when the
// watch is on. It adds to s (taken by TakeStamp before the file was read) the
// state of the configuration's descriptor files before validation read them
// (stampDescriptors). Only the file's own collectors and those of its
// collector files say which these are. A stamp of a file that could not be
// read has none. An exporter without the watch reads its configuration with
// Load: stamping the descriptor files reads them to learn what they import,
// for a stamp that no tick would ever compare.
func LoadStamped(path string, s *Stamp, opts ...LoadOption) (*model.Config, error) {
	return load(path, opts, func(c *model.Config) {
		descriptors := stampDescriptors(c)
		s.descriptors = &descriptors
	})
}

// UseStamp makes s, taken before the configuration in force was read, what
// the watch compares the configuration, its collector files and, when it was
// read with the stamp (LoadStamped), its descriptor files against.
func (m *Manager) UseStamp(s Stamp) {
	if s.found {
		m.lastMod, m.lastSize, m.lastMode = s.mod, s.size, s.mode
	}
	if s.entries != nil {
		m.watchedFiles, m.collectorFiles = s.entries, s.collectorFiles
	}
	if s.descriptors != nil {
		m.descriptors, m.descriptorsStamped = *s.descriptors, true
	}
}

// UseTargetsStamp is UseStamp for the static target file.
func (m *Manager) UseTargetsStamp(s Stamp) {
	if s.found {
		m.targetsLastMod, m.targetsLastSize, m.targetsLastMode = s.mod, s.size, s.mode
	}
}

// Get returns the configuration in force.
func (m *Manager) Get() *model.Config { return m.current.Load().config }

// InForce returns the configuration and the static target file in force
// (nil without one), as one reload left them. A caller that uses
// both, such as a static target scrape, reads them here once instead of
// separately, where a reload could happen between the two reads.
func (m *Manager) InForce() (*model.Config, *model.StaticTargetFile) {
	pair := m.current.Load()
	return pair.config, pair.targets
}

// OnInstall registers told to be called whenever a reload has put a
// configuration, a static target file or both in force. It is called once
// they are in force, before the reload returns or another begins, so what is
// kept elsewhere for a configuration follows it then, not when it is next
// used. told must not reload.
func (m *Manager) OnInstall(told func()) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.installed = append(m.installed, told)
}

// OnPrepare registers prepare to be called whenever a reload is about to put
// a configuration, a static target file or both in force: after they have
// been read and checked, and before they are in force. It gets the two as
// InForce will return them, including the one the reload leaves unchanged,
// and nil without a static target file. While it runs, Get and InForce still
// return what was in force, without waiting. So costly work that depends only
// on the configuration is done on the reload path, before anything can ask
// for it, and not by the first user of the configuration. The reload puts it
// in force later by as long as prepare takes. What prepare is given always
// goes in force when it returns, and the callbacks registered with OnInstall
// are then told. A refused reload prepares nothing. prepare is called under
// the lock that serializes reloads, as told is: it must not reload, nor
// register here or with OnInstall.
func (m *Manager) OnPrepare(prepare func(*model.Config, *model.StaticTargetFile)) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.prepared = append(m.prepared, prepare)
}

// SetWatchInterval enables the configuration watch and sets how often the
// files are re-stated. A non-positive interval leaves the watch disabled, which
// is the default: configuration is then read once at startup and changes take
// effect on restart.
//
// Turning the watch on stamps the descriptor files of the configuration in
// force as they are now, unless startup stamped them before reading them
// (UseStamp, as the exporter does). The first tick then reloads nothing that
// has not changed since. A manager whose watch stays off stamps none.
func (m *Manager) SetWatchInterval(interval time.Duration) {
	m.watchInterval = interval
	if m.WatchEnabled() && !m.descriptorsStamped {
		m.descriptors, m.descriptorsStamped = stampDescriptors(m.Get()), true
	}
}

// WatchEnabled reports whether ReloadLoop will do anything.
func (m *Manager) WatchEnabled() bool { return m.watchInterval > 0 }

// WatchInterval is how often an enabled watch re-stats the files, which is what
// bounds how stale a running configuration can be.
func (m *Manager) WatchInterval() time.Duration { return m.watchInterval }

// SetEnvExpansion records that the configuration was read with ${NAME}
// expansion, so a reload reads it the same way. A reload that quietly stopped
// expanding would replace a working configuration with one full of literal
// references.
func (m *Manager) SetEnvExpansion(expand bool) { m.expandEnv = expand }

// SetExporterInstanceName records the name the configuration was read with
// for otlp.exporter_instance (WithExporterInstanceName), so every reload, and
// in allocator mode every bundle, names the replica alike.
func (m *Manager) SetExporterInstanceName(name string) { m.exporterInstance = name }

// SetStaticTargetsEnvExpansion is SetEnvExpansion for the static target file.
func (m *Manager) SetStaticTargetsEnvExpansion(expand bool) { m.expandStaticTargetsEnv = expand }

// loadOptions returns the options the configuration was first read with.
func (m *Manager) loadOptions() []LoadOption {
	var options []LoadOption
	if m.expandEnv {
		options = append(options, WithEnvExpansion())
	}
	if m.exporterInstance != "" {
		options = append(options, WithExporterInstanceName(m.exporterInstance))
	}
	return options
}

// staticTargetsLoadOptions returns the options the static target file was
// first read with.
func (m *Manager) staticTargetsLoadOptions() []LoadOption {
	if m.expandStaticTargetsEnv {
		return []LoadOption{WithStaticTargetsEnvExpansion()}
	}
	return nil
}

// SetPythonPath records the interpreter used to check collector Python
// scripts, so a reloaded configuration is held to the same contract as the one
// the exporter started with.
func (m *Manager) SetPythonPath(path string) { m.pythonPath = path }

// SetTargets installs the static target document and the file it was read
// from. An empty path leaves the feature disabled.
func (m *Manager) SetTargets(path string, f *model.StaticTargetFile) {
	m.targetPath = path
	if f != nil {
		m.current.Store(&inForce{config: m.Get(), targets: f})
		m.syncDiscovery(f)
	}
	if path != "" && f != nil {
		m.Reloads.loaded(ReloadFileStaticTargets)
	}
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			m.targetsLastMod, m.targetsLastSize, m.targetsLastMode = st.ModTime(), st.Size(), st.Mode()
		}
	}
}

// StaticTargets returns the static targets currently in force.
func (m *Manager) StaticTargets() []model.StaticTarget {
	f := m.current.Load().targets
	if f == nil {
		return nil
	}
	return f.Targets
}

// StaticTargetFile returns the static target file in force, nil without one.
// A reload that changes the targets stores a new file, so the pointer tells a
// reader whether the targets changed since it last looked.
func (m *Manager) StaticTargetFile() *model.StaticTargetFile { return m.current.Load().targets }

// StaticTargetConcurrency is how many static targets are scraped at once, as
// the file in force says.
func (m *Manager) StaticTargetConcurrency() int {
	return m.current.Load().targets.ScrapeConcurrency()
}

// ReloadLoop watches the configuration files when the watch is enabled and
// returns immediately when it is not, so the opt-in costs nothing.
func (m *Manager) ReloadLoop(ctx context.Context) {
	if !m.WatchEnabled() {
		return
	}
	ticker := time.NewTicker(m.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reloadChanged()
			if watchTicked != nil {
				watchTicked()
			}
		}
	}
}

// watchTicked, when set, is called after every tick of a watch
// (SetWatchTicked).
var watchTicked func()

// SetWatchTicked registers ticked to be called by the watch loop after every
// tick, whatever the tick found, for managers whose loop starts from now on.
// It is called once the tick has reloaded what it found changed and logged
// it. It is for tests. A tick that finds nothing changed reads and logs
// nothing. So a test of an exporter in a child process that must show a tick
// reloaded nothing has nothing of the exporter's to wait for, and no fixed
// wait is long enough on a busy machine. It waits for the tick itself.
func SetWatchTicked(ticked func()) { watchTicked = ticked }

// The configuration is reloaded when the watch sees a file change
// (reloadChanged), on SIGHUP, and on POST /-/reload when the lifecycle API is
// enabled (Reload). Every reload goes through apply under reloadMu, so two
// triggers at once never interleave, and each is logged with what triggered
// it.
const (
	reloadTriggerWatch  = "watch"
	ReloadTriggerSignal = "sighup"
	ReloadTriggerHTTP   = "http"
)

// The configuration and the static target file are checked against each
// other: a target names a collector, and one exported over OTLP needs OTLP
// on. So a change that spans both, such as removing a collector and the
// target that uses it, is only valid as a whole. apply therefore reads the
// files it reloads together and installs them together when they agree. A
// file refused only because the other file, as in force, disagrees with it
// waits. When the other file changes, the watch reads it again together with
// that file, although its own modification time has been seen. Without this,
// a change written to both files, in either order or at once, left the
// configuration refused until it was touched again.

// reloadChanged reloads what the watch finds changed. It is one tick of the
// watch: whatever changed since the last tick, there is one reload. It
// reloads:
//   - the configuration, when the file or one of its collector files
//     changed, or a descriptor file of the configuration in force, or, after
//     a refused reload, a file it names or one of those imports;
//   - the static target file, when it changed, or, after a refused reload, a
//     file its check opens, or together with the configuration when a
//     descriptor file in force changed and the target file is checked
//     against descriptor files (targetsFollowDescriptors);
//   - with either, the other file when it waits for it.
//
// A target file that waits for the configuration is read again when the
// configuration changes (configChanged), not when a descriptor file of the
// configuration in force does. The configuration read for that file is the
// one that refused it, and a descriptor file opened by the target file's own
// check is among the files it is retried for (targetsChanged).
func (m *Manager) reloadChanged() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	configChanged, descriptorsChanged, targetsChanged := m.configChanged(), m.descriptors.changed(), m.targetsChanged()
	doConfig := configChanged || descriptorsChanged || targetsChanged && m.configWaits
	doTargets := targetsChanged || configChanged && m.targetsWaits || descriptorsChanged && m.targetsFollowDescriptors()
	// A file refused because a file it reads could not be read is read again
	// at every tick, even if nothing changed. So is the other file when it
	// waits for that one. When read only for that reason, it is quiet: the
	// same refusal again is neither logged nor counted.
	m.configQuiet = !doConfig && (m.configUnread || m.configWaits && m.targetsUnread && !doTargets)
	m.targetsQuiet = !doTargets && (m.targetsUnread || m.targetsWaits && m.configUnread && !doConfig)
	defer func() { m.configQuiet, m.targetsQuiet = false, false }()
	doConfig, doTargets = doConfig || m.configQuiet, doTargets || m.targetsQuiet
	if !doConfig && !doTargets {
		return
	}
	_ = m.apply(reloadTriggerWatch, doConfig, doTargets)
}

// configChanged reports whether the configuration changed since it was last
// read: the file, one of its collector files, or, while the last read
// configuration is refused, a file it names or one of those imports. The
// descriptor files of the configuration in force, which the watch also
// reloads for, are checked separately (descriptors). Unlike these, they do
// not make a target file that waits for the configuration be read again
// (reloadChanged).
func (m *Manager) configChanged() bool {
	st, err := os.Stat(m.path)
	if err != nil {
		return false
	}
	// A collector file edited, added or removed is a change too, even if the
	// configuration file itself is untouched. So, while the last read
	// configuration is refused, is a file it names (retryFiles) or a
	// descriptor file it was checked against, as that file was before the
	// refused load read it (loading). The files are only stamped: a tick that
	// finds them unchanged reads and logs nothing.
	return !st.ModTime().Equal(m.lastMod) || st.Size() != m.lastSize || st.Mode() != m.lastMode || collectorFilesStamp(m.path, m.watchedFiles) != m.collectorFiles ||
		len(m.retryFiles) > 0 && filesStamp(m.retryFiles) != m.retryStamp || m.loading.changed()
}

// targetsFollowDescriptors reports whether the static target file in force
// is read again with the configuration when any descriptor file of the
// configuration in force changed. That is so when one of its targets sets a
// request.message for a collector that names descriptor files, which the
// message is checked against (targetsChecked). The tick then reads the
// target file with the configuration, so a message the new descriptors
// refuse is reported for the target file, on its own line, and not only as
// what the configuration disagrees with. Which descriptor file changed is not
// checked: there is one stamp for all of them, so a target file with such a
// target is also read for a file of a collector none of its targets uses, and
// is then found unchanged. A target file whose last reload was refused is not
// the one in force, and is read again only for what it was refused for
// (targetsChanged, targetsWaits).
func (m *Manager) targetsFollowDescriptors() bool {
	c, f := m.InForce()
	return m.targetPath != "" && f != nil &&
		!slices.Contains(m.Reloads.Rejected(), ReloadFileStaticTargets) && len(targetsNamedFiles(f, c)) > 0
}

// targetsChanged reports whether the static target file changed since it was
// last read or, while the last read file is refused, whether a file its check
// opened changed (targetsRetryFiles). As for the configuration, a tick that
// finds them unchanged logs nothing.
func (m *Manager) targetsChanged() bool {
	if m.targetPath == "" {
		return false
	}
	st, err := os.Stat(m.targetPath)
	return err == nil && (!st.ModTime().Equal(m.targetsLastMod) || st.Size() != m.targetsLastSize || st.Mode() != m.targetsLastMode ||
		len(m.targetsRetryFiles) > 0 && filesStamp(m.targetsRetryFiles) != m.targetsRetryStamp)
}

// Reload reloads the configuration, and the static target file when there
// is one, now, whether or not they changed, and returns why either was
// rejected. A rejected file leaves the previous one in force.
func (m *Manager) Reload(trigger string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.apply(trigger, true, m.targetPath != "")
}

// apply reads the configuration when doConfig and the static target file when
// doTargets, and installs what is valid. reloadMu is held.
func (m *Manager) apply(trigger string, doConfig, doTargets bool) error {
	var errs []error
	var cfg *model.Config
	var targets *model.StaticTargetFile
	if doConfig {
		c, err := m.loadConfig()
		if err != nil {
			errs = append(errs, m.rejectConfig(trigger, err, false))
		}
		cfg = c
	}
	if doTargets {
		f, err := m.loadTargets()
		if err != nil {
			errs = append(errs, m.rejectTargets(trigger, err, false))
		}
		targets = f
	}
	if cfg == nil && targets == nil {
		return errors.Join(errs...)
	}
	// Together: each read file with the other as read, or as in force. The
	// checks only read the pair in force, which the scrapes are reading.
	pairConfig, pairTargets := m.InForce()
	if cfg != nil {
		pairConfig = cfg
	}
	if targets != nil {
		// The targets its discovered_targets make of what their sources last
		// found are put in force with it.
		targets = m.discovered(targets, pairConfig)
		pairTargets = targets
		// The files the checks below open for the file just read are stamped
		// before they are read, as the configuration's are, and kept if it is
		// refused. One of them back in place is then a change. With the watch
		// on, the same goes for the files those import, read here to learn
		// which they are (stampDescriptors). One that changes after the checks
		// read it, and before the file is refused, is then a change too.
		// The configuration in force is the one read with it when no other
		// was read, and is then looked through once.
		inForce := m.Get()
		if inForce == pairConfig {
			inForce = nil
		}
		m.targetsRetryChecked = targetsChecked(targets, pairConfig, inForce)
		m.targetsRetryFiles = namedFiles(m.targetsRetryChecked)
		m.targetsRetryStamp = filesStamp(m.targetsRetryFiles)
		if m.WatchEnabled() {
			m.targetsRetryFiles, m.targetsRetryStamp, _ = retryImported(m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked)
		}
	}
	// A pair asked about again below is not checked again: its verdict is
	// the one it got (pairVerdicts).
	var verdicts pairVerdicts
	if verdicts.agree(pairTargets, pairConfig) == nil {
		m.install(trigger, cfg, targets)
		return errors.Join(errs...)
	}
	// They disagree. When both were read, one may still go alone, with the
	// other in force; what is left is refused, and waits for the other file.
	if cfg != nil && verdicts.agree(m.StaticTargetFile(), cfg) == nil {
		m.install(trigger, cfg, nil)
		cfg = nil
	}
	if targets != nil && verdicts.agree(targets, m.Get()) == nil {
		m.install(trigger, nil, targets)
		targets = nil
	}
	if cfg != nil {
		errs = append(errs, m.rejectConfig(trigger, verdicts.agree(m.StaticTargetFile(), cfg), true))
	}
	if targets != nil {
		errs = append(errs, m.rejectTargets(trigger, verdicts.agree(targets, m.Get()), true))
	}
	return errors.Join(errs...)
}

// pairVerdicts are the verdicts of the checks one reload made, each of a
// static target file against a configuration, identified by the two values
// themselves. A reload asks about up to three pairs, some up to three times:
// the pair it read; a file it read with the other in force, which is the
// pair it read again once the other went in force alone; and the pair a
// refused file is refused for, to say why. Each check costs microseconds per
// target, under the reload lock. A pair asked about again gets the very same
// error it got. They are kept for one reload only: the next reads new files
// and checks what is in force again.
type pairVerdicts []pairVerdict

type pairVerdict struct {
	targets *model.StaticTargetFile
	config  *model.Config
	err     error
}

// agree is agree(f, c), checked the first time the pair is asked about.
func (v *pairVerdicts) agree(f *model.StaticTargetFile, c *model.Config) error {
	for _, seen := range *v {
		if seen.targets == f && seen.config == c {
			return seen.err
		}
	}
	err := agree(f, c)
	*v = append(*v, pairVerdict{targets: f, config: c, err: err})
	return err
}

// agree checks a static target file against a configuration; no file agrees
// with every configuration.
func agree(f *model.StaticTargetFile, c *model.Config) error {
	if f == nil {
		return nil
	}
	return ValidateStaticTargetsAgainst(f, c)
}

// watchCollectorFiles makes entries the collector files the watch stamps.
func (m *Manager) watchCollectorFiles(entries []string) {
	m.watchedFiles = entries
	m.collectorFiles = collectorFilesStamp(m.path, entries)
}

// listedCollectorFiles is the collector_files a configuration file lists,
// read without the rest of it, which may be what is wrong with it.
func listedCollectorFiles(path string, opts []LoadOption) ([]string, bool) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, false
	}
	var listed struct {
		CollectorFiles []string `yaml:"collector_files"`
	}
	if yaml.Unmarshal(b, &listed) != nil {
		return nil, false
	}
	return listed.CollectorFiles, true
}

// loadConfig reads and checks the configuration on its own. reloadMu is held.
func (m *Manager) loadConfig() (*model.Config, error) {
	if st, err := os.Stat(m.path); err == nil {
		m.lastMod, m.lastSize, m.lastMode = st.ModTime(), st.Size(), st.Mode()
	}
	// The collector files watched from now are the ones this file lists,
	// read on its own, so a refused file that added one is read again
	// when that one is fixed; a file that cannot say keeps the old list.
	entries := m.Get().CollectorFiles
	if listed, ok := listedCollectorFiles(m.path, m.loadOptions()); ok {
		entries = listed
	}
	m.watchCollectorFiles(entries)
	// The files the configuration names are stamped before the load opens
	// them, as the configuration file is, so one replaced while it was read
	// is a change for the next tick. These are the files of the configuration
	// in force, and with them those of the one being read as soon as it says
	// which. The descriptor files of the configuration in force are stamped
	// again with them. If this load is refused they stay the watched ones, and
	// a tick reloads for them when they change again, not at every tick for
	// the change just read. The descriptor files of the configuration being
	// read are stamped only with the watch on (loading): without it no tick
	// compares them, and no file is read to learn what it imports.
	inForce := namedFiles(m.Get())
	m.retryFiles, m.retryStamp = inForce, filesStamp(inForce)
	m.retryConfigs, m.retryImports = []*model.Config{m.Get()}, false
	m.descriptors.stamp, m.loading = filesStamp(m.descriptors.files), stampedFiles{}
	c, err := load(m.path, m.loadOptions(), func(candidate *model.Config) {
		m.retryFiles = namedFiles(m.Get(), candidate)
		m.retryStamp = filesStamp(m.retryFiles)
		m.retryConfigs = []*model.Config{m.Get(), candidate}
		if m.WatchEnabled() {
			m.loading = stampDescriptors(candidate)
		}
	})
	if err != nil {
		return nil, err
	}
	if err := ValidatePythonScripts(m.pythonPath, c); err != nil {
		return nil, err
	}
	return c, nil
}

// loadTargets reads and checks the static target file on its own. reloadMu is
// held.
func (m *Manager) loadTargets() (*model.StaticTargetFile, error) {
	if st, err := os.Stat(m.targetPath); err == nil {
		m.targetsLastMod, m.targetsLastSize, m.targetsLastMode = st.ModTime(), st.Size(), st.Mode()
	}
	// Reading the file on its own opens no other: one refused here is read
	// again when it changes, and for nothing else.
	m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked = nil, "", nil
	if m.absentTargets && targetsAbsent(m.targetPath) {
		return &model.StaticTargetFile{}, nil
	}
	f, err := LoadStaticTargets(m.targetPath, m.staticTargetsLoadOptions()...)
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// install puts c and f in force in one step; either may be nil to keep the
// one in force. No reader sees this reload's configuration with the last
// reload's targets, or the reverse, which could name a collector the other
// no longer has. The callbacks registered with OnPrepare run first, and
// nothing comes between their preparing and the values going in force. What
// follows from each is done once both are in force, and then the callbacks
// registered with OnInstall are told. reloadMu is held.
func (m *Manager) install(trigger string, c *model.Config, f *model.StaticTargetFile) {
	before := *m.current.Load()
	pair := before
	if c != nil {
		pair.config = c
	}
	if f != nil {
		pair.targets = f
	}
	// The generation is the configuration's: a static target file put in
	// force alone leaves it as it was.
	if c != nil {
		pair.generation = m.generation
	}
	for _, prepare := range m.prepared {
		prepare(pair.config, pair.targets)
	}
	// What each file changed is worked out against what was in force before
	// it, and before it is in force, with the fingerprints just prepared
	// (reloadchanges.go).
	var collectorsChanged, targetsChanged string
	var changed []ReloadChangeList
	if c != nil {
		lists := collectorChanges(before.config, c, m.fingerprint)
		collectorsChanged = reloadChanges("collectors", lists)
		changed = append(changed, reloadChangeLists("collectors", lists)...)
	}
	if f != nil {
		lists := targetChanges(before.targets, f)
		targetsChanged = reloadChanges("static targets", lists)
		changed = append(changed, reloadChangeLists("static targets", lists)...)
	}
	m.current.Store(&pair)
	if c != nil {
		m.awaiting.Store(false)
		m.installedConfig(trigger, c, collectorsChanged)
	}
	if f != nil {
		m.installedTargets(trigger, f, targetsChanged)
		m.syncDiscovery(f)
	}
	if trigger != discoveryTrigger {
		m.lastReload.Store(&ReloadSummary{At: time.Now(), Changes: joinReloadChanges(collectorsChanged, targetsChanged), Lists: changed})
	}
	for _, told := range m.installed {
		told()
	}
}

// installedConfig records and logs that c was put in force, with changes,
// the collectors it added, removed and changed, and lets what is kept for a
// configuration follow it. reloadMu is held.
func (m *Manager) installedConfig(trigger string, c *model.Config, changes string) {
	m.configWaits, m.configUnread, m.configRefusal = false, false, ""
	// Once in force, the configuration is not read again for a certificate
	// or credential file it names: whatever uses one reads it again itself.
	// It is read again for its descriptor files, which it is checked
	// against: they are watched from now on as they were before this load
	// read them.
	m.retryFiles, m.retryStamp, m.retryConfigs, m.retryImports = nil, "", nil, false
	m.descriptors, m.loading = m.loading, stampedFiles{}
	m.Reloads.record(reloadFileConfig, true)
	// Interpreters of scripts this reload removed or changed are stopped now
	// rather than after the idle timeout.
	transform.PythonWorkers().Retain(transform.PythonWorkerKeys(m.pythonPath, c))
	// The new configuration may list other collector files than loadConfig
	// took them to be; only then are they stamped again, since stamping them
	// now would make a change written while they were read part of what was.
	if !slices.Equal(m.watchedFiles, c.CollectorFiles) {
		m.watchCollectorFiles(c.CollectorFiles)
	}
	LogNotices(m.logger, m.path, c)
	m.logger.Info("configuration reloaded", "trigger", trigger, "collectors", len(c.Collectors), "collector_files", len(c.LoadedCollectorFiles), "changes", changes)
}

// installedTargets records and logs that f was put in force, with changes,
// the static targets it added, removed and changed. reloadMu is held.
func (m *Manager) installedTargets(trigger string, f *model.StaticTargetFile, changes string) {
	if trigger == discoveryTrigger {
		m.logger.Info("discovered static targets changed", "targets", len(f.Targets), "discovered", len(f.Targets)-f.Written, "changes", changes)
		return
	}
	m.targetsWaits, m.targetsUnread, m.targetsRefusal = false, false, ""
	m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked = nil, "", nil
	m.Reloads.record(ReloadFileStaticTargets, true)
	m.logger.Info("static targets reloaded", "trigger", trigger, "targets", len(f.Targets), "changes", changes)
}

// rejectConfig refuses a configuration, which stays as it was. waits says it
// was refused only by the static target file in force, and is read again
// when that file changes. A refusal because a file could not be read is
// retried at every tick (configUnread). A retry that gets the same text as
// the last refusal (configQuiet) is not a new refusal, and is neither logged
// nor counted. So the log line and the failed-reload count report each
// distinct refusal once, as they do when a tick finds nothing changed and
// reads nothing. reloadMu is held.
func (m *Manager) rejectConfig(trigger string, err error, waits bool) error {
	m.configWaits = waits
	same := model.SameFailureText(err)
	quiet := m.configQuiet && same == m.configRefusal
	m.configUnread, m.configRefusal = !waits && unreadable(err), same
	// With the watch on, the descriptor files of the configuration that was
	// read, named and imported, were stamped before it was validated, and
	// stay watched in that state (loading). If they were stamped again now, a
	// file fixed since the load read it would be stamped as fixed, and the
	// refused configuration would never be read again. Only the imports of
	// the configuration in force, which this load need not have read, are
	// looked at here.
	configs := m.retryConfigs
	if m.WatchEnabled() {
		configs = configs[:1]
	}
	m.retryFiles, m.retryStamp, m.retryImports = retryImported(m.retryFiles, m.retryStamp, configs...)
	m.retryImports = m.retryImports || m.loading.imports
	// The file is named, as a collector file's error names its own: a YAML
	// error's line number means nothing without it. Problems that are all
	// in one collector file are logged against that file.
	attrs := []any{"trigger", trigger, "file", problemFile(err, m.path), "error", err}
	var stamped []string
	if len(m.retryFiles) > 0 {
		stamped = append(stamped, "a file it names")
	}
	if m.retryImports {
		stamped = append(stamped, "a file one of those imports")
	}
	attrs = m.retriedWhen(attrs, "the configuration", stamped, "the static target file", waits, m.configUnread)
	if !quiet {
		m.logger.Error("configuration reload rejected", attrs...)
		m.Reloads.record(reloadFileConfig, false)
	}
	return fmt.Errorf("configuration %s: %w", m.path, err)
}

// rejectTargets is rejectConfig for the static target file.
func (m *Manager) rejectTargets(trigger string, err error, waits bool) error {
	m.targetsWaits = waits
	same := model.SameFailureText(err)
	quiet := m.targetsQuiet && same == m.targetsRefusal
	m.targetsUnread, m.targetsRefusal = !waits && unreadable(err), same
	// The files its check opens are those the collectors name and those
	// that these import, which with the watch on were stamped before the
	// check read them (apply).
	if !m.WatchEnabled() {
		m.targetsRetryFiles, m.targetsRetryStamp, _ = retryImported(m.targetsRetryFiles, m.targetsRetryStamp, m.targetsRetryChecked)
	}
	attrs := []any{"trigger", trigger, "file", m.targetPath, "error", err}
	var stamped []string
	if len(m.targetsRetryFiles) > 0 {
		stamped = append(stamped, "a file its check opens")
	}
	attrs = m.retriedWhen(attrs, "the static target file", stamped, "the configuration", waits, m.targetsUnread)
	if !quiet {
		m.logger.Error("static target reload rejected", attrs...)
		m.Reloads.record(ReloadFileStaticTargets, false)
	}
	return fmt.Errorf("static target file %s: %w", m.targetPath, err)
}

// retryImported adds to the files stamped for a refused file, and to their
// stamp, the files that the .proto files among them import, as found by the
// load or check that refused it (importedFiles). So a file refused because
// an imported file was missing or did not compile is retried when that file
// appears or changes. imports says there are such files. When one of them
// changed after it was read, the stamp matches no files, and the next tick
// reads the refused file again.
func retryImported(files []string, stamp string, configs ...*model.Config) (all []string, stamped string, imports bool) {
	imported, importedStamp, settled := importedFiles(files, configs...)
	if len(imported) == 0 {
		return files, stamp, false
	}
	all = append(slices.Clip(files), imported...)
	if !settled {
		return all, "", true
	}
	return all, stamp + importedStamp, true
}

// retriedWhen adds retried_when to a refused file's log line: what makes the
// watch read the file again. That is the file itself changing (for the
// configuration, including its collector files), each kind of file stamped
// for it while it is refused (retryFiles, targetsRetryFiles) when there are
// any, and the other file when this one waits for it. Each is named only
// when it applies, so the line never suggests that one change alone helps,
// nor promises a retry for a file nothing watches. A file refused because a
// file it reads could not be read (unread) is also read again at every tick
// until it can be, and the line says so after the changes. Without the
// watch nothing is read again until a reload is requested, and the line
// says nothing.
func (m *Manager) retriedWhen(attrs []any, file string, stamped []string, other string, waits, unread bool) []any {
	if !m.WatchEnabled() {
		return attrs
	}
	whens := append([]string{file}, stamped...)
	if waits {
		whens = append(whens, other)
	}
	when := whens[0]
	if last := len(whens) - 1; last > 0 {
		when = strings.Join(whens[:last], ", ") + " or " + whens[last]
	}
	when += " changes"
	if unread {
		when += ", and at every tick while a file it reads cannot be read"
	}
	return append(attrs, "retried_when", when)
}

// checkPythonLibrary rejects a declared library the image does not install.
// BeautifulSoup gets its own message: it was installed until lxml.html took
// over its job, so a configuration written for it needs pointing somewhere.
func checkPythonLibrary(collector, lib string) error {
	if transform.PythonLibraries[lib] {
		return nil
	}
	if lib == "beautifulsoup4" || lib == "bs4" {
		return fmt.Errorf("collector %q declares Python library %q, which the image no longer installs; parse HTML with lxml.html instead and declare lxml", collector, lib)
	}
	return fmt.Errorf("collector %q declares unsupported Python library %q; the supported libraries are lxml, PyYAML and python-dateutil", collector, lib)
}

// LogNotices logs what Validate recorded about a loaded configuration
// without refusing it: once per deprecated spelling it used and once per
// warning, so the operator hears about each on every start and reload until it
// is changed.
func LogNotices(logger *slog.Logger, path string, c *model.Config) {
	for _, message := range c.Deprecations {
		logger.Warn("deprecated configuration", "file", path, "deprecation", message)
	}
	for _, message := range c.Warnings {
		logger.Warn("configuration warning", "file", path, "warning", message)
	}
}

// undecidedDecoderWarning says how a collector without a decoder.type, whose
// transform implies none, decodes what it reads.
func undecidedDecoderWarning(x *model.Collector) string {
	how := "by the Content-Type header of each response"
	if requestTypeOf(x).FileExtensions {
		how = "by the extension of each file"
	}
	return fmt.Sprintf("collector %q sets no decoder.type, so it decodes %s, and by the content when that does not say; set decoder.type to fix the decoder", x.Name, how)
}

// checkErrorPolicy refuses a policy that is not fail, log or ignore as
// written: lower case, without surrounding blanks, the only spelling the
// schemas accept. So a file an editor flags is one the exporter refuses, and
// the other way round.
func checkErrorPolicy(collector, key, value string) error {
	switch value {
	case model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore:
		return nil
	}
	return fmt.Errorf("collector %q %s has invalid value %q; want fail, log or ignore, in lower case", collector, key, value)
}

// validateFailureBackoff checks failure_backoff and fills in its defaults:
// after 3, initial 30s, max 10m (or initial when that is longer, so a block
// that sets only initial above 10m is not refused for a max it did not
// write), and min_budget 1s. A block with enabled: false is checked the same
// way, as a schema checks it, and then removed. The collector then backs off
// from nothing, and on a reload and in the cache key it compares as one
// without the block, which is how it scrapes. An enabled block is kept
// without enabled, since both spellings are the same.
func validateFailureBackoff(c *model.Collector) error {
	b := c.FailureBackoff
	if b == nil {
		return nil
	}
	if err := checkFailureBackoff(c.Name, b); err != nil {
		return err
	}
	if model.BackoffSwitchedOff(b) {
		c.FailureBackoff = nil
	}
	b.Enabled = nil
	return nil
}

// checkFailureBackoff is validateFailureBackoff's check of the block of
// the collector named, filling in its defaults.
func checkFailureBackoff(name string, b *model.FailureBackoffConfig) error {
	if b.After == nil {
		after := model.DefaultFailureBackoffAfter
		b.After = &after
	} else if *b.After < 1 {
		return fmt.Errorf("collector %q failure_backoff.after must be at least 1, the consecutive failed scrapes of a target before its scrapes stop contacting it; got %d", name, *b.After)
	}
	if b.Initial == nil {
		initial := model.Duration(model.DefaultFailureBackoffInitial)
		b.Initial = &initial
	} else if *b.Initial <= 0 {
		return fmt.Errorf("collector %q failure_backoff.initial must be a positive duration, such as 30s; got %s", name, time.Duration(*b.Initial))
	}
	if b.Max == nil {
		longest := max(model.Duration(model.DefaultFailureBackoffMax), *b.Initial)
		b.Max = &longest
	} else if *b.Max < *b.Initial {
		return fmt.Errorf("collector %q failure_backoff.max %s must not be shorter than failure_backoff.initial %s", name, time.Duration(*b.Max), time.Duration(*b.Initial))
	}
	if b.MinBudget == nil {
		floor := model.Duration(model.DefaultFailureBackoffMinBudget)
		b.MinBudget = &floor
	} else if *b.MinBudget <= 0 {
		return fmt.Errorf("collector %q failure_backoff.min_budget must be a positive duration, the least budget a trip that timed out must have had to count as the target's failure, such as 1s; got %s", name, time.Duration(*b.MinBudget))
	}
	return nil
}

// validateCache checks a collector's cache when the configuration loads.
func validateCache(c *model.Collector) error {
	if c.Cache.TTL < 0 {
		return fmt.Errorf("collector %q cache.ttl must not be negative", c.Name)
	}
	if c.Cache.StaleIfError < 0 {
		return fmt.Errorf("collector %q cache.stale_if_error must not be negative", c.Name)
	}
	return nil
}
