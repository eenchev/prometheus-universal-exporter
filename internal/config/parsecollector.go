package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The debug page (exporter/debugpage.go) runs a collector written in a
// browser against a response, without putting it in force. The collector is
// read here, as the loader reads one of the collectors list, so that what the
// debug page accepts the configuration accepts and the other way round, and
// a mistake is reported in the words, and at the line, a load would report it.
// Three things a load does are not done:
//
//   - ${NAME} references are never expanded, whatever --config.expand-env
//     says: the debug page's answer would show the exporter's environment;
//   - no file the collector names is opened, as WithoutNamedFiles loads;
//   - the expression caches are not sized for it (expr.ReserveExpressions),
//     which is the configuration in force's to do: a debug page's collector
//     sizing them to its own few expressions would have the configuration's
//     compiled again and again.

// ParseCollector reads document, one collector as it is written under
// collectors: — a mapping that starts with its name — and checks it as Load
// checks a collector of the configuration. refuse, when not nil, is given the
// collector as written, before it is checked, and may refuse it: the
// debug page refuses what it does not run. The configuration returned holds
// the collector alone, its defaults filled in, with the warnings a load would
// give for it.
func ParseCollector(document []byte, refuse func(*model.Collector) error) (*model.Config, error) {
	if err := refuseNotScalarKeys(document); err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(document, &doc); err != nil {
		return nil, yamlError(err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind == yaml.ScalarNode && doc.Content[0].Tag == "!!null" {
		return nil, errors.New("the collector is empty; write one collector as it is written under collectors:, a mapping that starts with its name")
	}
	root := doc.Content[0]
	switch root.Kind {
	case yaml.SequenceNode:
		return nil, fmt.Errorf("line %d: write one collector as a mapping, without the dash that starts an entry of the collectors list", root.Line)
	case yaml.MappingNode:
		for _, entry := range model.MappingEntries(root) {
			if key := model.KeyName(entry.Key); key == collectorFileKey || key == "collector_files" {
				return nil, fmt.Errorf("line %d: %q is a key of the configuration, not of a collector; write one collector as it is written under collectors:, a mapping that starts with its name", entry.Key.Line, key)
			}
		}
	}
	var x model.Collector
	dec := yaml.NewDecoder(bytes.NewReader(document))
	dec.KnownFields(true)
	if err := withValueProblems(dec.Decode(&x), document, &x); err != nil && !errors.Is(err, io.EOF) {
		return nil, yamlError(err)
	}
	if err := oneDocument(dec); err != nil {
		return nil, err
	}
	if refuse != nil {
		if err := refuse(&x); err != nil {
			return nil, err
		}
	}
	c := &model.Config{Collectors: []model.Collector{x}}
	collector := &c.Collectors[0]
	if err := checkCollectorName(collector); err != nil {
		return nil, err
	}
	collector.DescriptorFilesUnread = true
	if err := validateCollector(c, collector); err != nil {
		return nil, err
	}
	return c, nil
}
