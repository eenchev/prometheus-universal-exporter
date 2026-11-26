package model

// The types of an http collector's chained requests: request.extract,
// request.then and the collector's answer (fetch/chain.go runs them).

// ChainStep is one entry of request.then: a request made after the
// collector's own, to the same target, which may send values read from the
// answers before it.
type ChainStep struct {
	// Name names the step in errors, metrics and the answer document.
	Name string `yaml:"name" json:"name"`
	// Method is GET when left out, or POST when the step has a body.
	Method string `yaml:"method" json:"method,omitempty"`
	// Path is a path on the target, joined to it as request.path is; the
	// collector's request.path when left out. URL is one placeholder of an
	// extracted link, sent in place of the path.
	Path string `yaml:"path" json:"path,omitempty"`
	URL  string `yaml:"url" json:"url,omitempty"`
	// Query and Headers are added to the step's request, each replacing
	// one of the same name.
	Query   map[string]string `yaml:"query" json:"query,omitempty"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
	// Body, Form and JSON are the step's body, at most one of them: text,
	// a form, or a JSON object of text values.
	Body string            `yaml:"body" json:"body,omitempty"`
	Form map[string]string `yaml:"form" json:"form,omitempty"`
	JSON map[string]string `yaml:"json" json:"json,omitempty"`
	// ForwardedHeaders, nil or true, sends the step the forwarded headers,
	// header_<name> probe parameters and request.forward_headers among
	// them; false sends it none of them but a forwarded Authorization and
	// Host.
	ForwardedHeaders *bool `yaml:"forwarded_headers" json:"forwarded_headers,omitempty"`
	// AcceptStatus replaces the collector's request.accept_status for the
	// step.
	AcceptStatus []string `yaml:"accept_status" json:"accept_status,omitempty"`
	// Extract are the values read from the step's answer.
	Extract map[string]ChainExtract `yaml:"extract" json:"extract,omitempty"`
	// ForEach names a list value read earlier: the step makes one request
	// per entry. Concurrency, MaxItems and OnItemError apply to such a
	// step alone; 0 and "" are their defaults.
	ForEach     string `yaml:"for_each" json:"for_each,omitempty"`
	Concurrency int    `yaml:"concurrency" json:"concurrency,omitempty"`
	MaxItems    int    `yaml:"max_items" json:"max_items,omitempty"`
	OnItemError string `yaml:"on_item_error" json:"on_item_error,omitempty"`
}

// ChainExtract is where an answer holds a value, exactly one of the places
// a steps login reads one from (LoginExtract), and whether a value not
// found is empty instead of a failure (Optional), and masked where it is
// shown (Secret). A json value may also be a list, for for_each.
type ChainExtract struct {
	Regex    string `yaml:"regex" json:"regex,omitempty"`
	JSON     string `yaml:"json" json:"json,omitempty"`
	Form     string `yaml:"form" json:"form,omitempty"`
	Header   string `yaml:"header" json:"header,omitempty"`
	Cookie   string `yaml:"cookie" json:"cookie,omitempty"`
	Optional bool   `yaml:"optional" json:"optional,omitempty"`
	Secret   bool   `yaml:"secret" json:"secret,omitempty"`
}

// Place is where the value is read from, as a steps login names it.
func (e ChainExtract) Place() LoginExtract {
	return LoginExtract{Regex: e.Regex, JSON: e.JSON, Form: e.Form, Header: e.Header, Cookie: e.Cookie}
}

// The limits and defaults of a chain, and the values of answer and
// on_item_error.
const (
	// ChainFirst is the name of the collector's own request, the chain's
	// first, which no step may take.
	ChainFirst              = "first"
	MaxChainSteps           = 12
	MaxChainValues          = 16
	DefaultChainConcurrency = 4
	MaxChainConcurrency     = 16
	DefaultChainMaxItems    = 100
	MaxChainMaxItems        = 1000
	ChainAnswerLast         = "last"
	ChainAnswerAll          = "all"
	ChainItemFail           = "fail"
	ChainItemSkip           = "skip"
)

// ChainAnswers and ChainItemErrors are the values of answer and
// on_item_error.
var (
	ChainAnswers    = []string{ChainAnswerLast, ChainAnswerAll}
	ChainItemErrors = []string{ChainItemFail, ChainItemSkip}
)

// ChainStepNamePattern is what a step's name must match.
const ChainStepNamePattern = `^[a-z_][a-z0-9_]*$`
