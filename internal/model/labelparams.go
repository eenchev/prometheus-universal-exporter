package model

// A collector's fixed label values may hold {{param_<name>}} placeholders,
// which a probe's parameters fill as they fill the request's
// (fetch/labelparams.go): the values of transform.labels and the value of a
// metric rule's static label. Whether a collector has any is decided once,
// when the configuration loads, and kept here in its parsed form: a probe of
// a collector without one, as nearly every collector is, reads a nil pointer
// and does nothing more.

// LabelParams is the label values of a collector that hold a placeholder,
// each parsed. It is made when the configuration loads and never changed:
// the probes of a collector share it.
type LabelParams struct {
	// Collector are the values of transform.labels, in the order of their
	// names, and Rules those of the metric rules' static labels, in the
	// order of the rules and of each rule's labels.
	Collector []LabelTemplate
	Rules     []LabelTemplate
}

// LabelTemplate is one label value with its placeholders.
type LabelTemplate struct {
	// Name is the label's name. Rule and Label say which label of which
	// metric rule the value is, counted from 0; a value of transform.labels
	// has neither, and is found by its name.
	Name        string
	Rule, Label int
	// Where names the value as an error names it: transform.labels.tenant,
	// or metric "up" label "tenant" value.
	Where string
	// Text is the value as it is written, and Placeholders where in it each
	// placeholder stands, in their order.
	Text         string
	Placeholders []LabelPlaceholder
}

// LabelPlaceholder is one {{param_<name>}} or {{param_<name>:<default>}} of
// a label value: the probe parameter that fills it, its default, and the
// bytes of the value it takes up, braces included.
type LabelPlaceholder struct {
	Param      string
	Default    string
	HasDefault bool
	Start, End int
}
