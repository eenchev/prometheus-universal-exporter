package allocator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// TargetGroup is a target group of Prometheus's file_sd and HTTP service
// discovery: targets sharing labels.
type TargetGroup struct {
	Targets []string          `json:"targets" yaml:"targets"`
	Labels  map[string]string `json:"labels" yaml:"labels"`
}

// discovered is one target a source found: its address and its labels,
// the source's own and those its labels mapping set.
type discovered struct {
	Address string            `json:"address"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// parseGroupsJSON reads a JSON list of target groups, strictly: a key
// Prometheus would refuse is refused.
func parseGroupsJSON(raw []byte) ([]TargetGroup, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var groups []TargetGroup
	if err := dec.Decode(&groups); err != nil {
		return nil, fmt.Errorf("not a JSON list of target groups: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a JSON list of target groups: more follows the list")
	}
	return groups, checkGroups(groups)
}

// parseGroupsYAML reads a YAML list of target groups, strictly.
func parseGroupsYAML(raw []byte) ([]TargetGroup, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var groups []TargetGroup
	if err := dec.Decode(&groups); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("not a YAML list of target groups: %w", err)
	}
	return groups, checkGroups(groups)
}

// checkGroups refuses a group with an empty target or a label whose name
// is no label name.
func checkGroups(groups []TargetGroup) error {
	for i := range groups {
		for _, target := range groups[i].Targets {
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("target group %d has an empty target", i)
			}
		}
		for name := range groups[i].Labels {
			if !model.ValidLabelName(name) {
				return fmt.Errorf("target group %d has the label %q, which is not a label name", i, name)
			}
		}
	}
	return nil
}

// flatten is the targets of groups, each with its group's labels and the
// labels mapping applied, refused when there are more than limit.
func flatten(groups []TargetGroup, mapping map[string]string, limit int) ([]discovered, error) {
	n := 0
	for i := range groups {
		n += len(groups[i].Targets)
	}
	if n > limit {
		return nil, fmt.Errorf("the answer holds %d targets, more than the %d max_targets allows", n, limit)
	}
	out := make([]discovered, 0, n)
	for _, g := range groups {
		for _, target := range g.Targets {
			out = append(out, discovered{Address: target, Labels: mapLabels(g.Labels, mapping)})
		}
	}
	return out, nil
}

// mapLabels is labels with mapping applied: each label mapping names set
// to the value of the source label it names, when that is not empty.
func mapLabels(labels, mapping map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels)+len(mapping))
	for name, value := range labels {
		out[name] = value
	}
	for name, from := range mapping {
		if value := labels[from]; value != "" {
			out[name] = value
		}
	}
	return out
}

// readBounded reads r up to maxDiscoveryAnswerBytes, refusing more.
func readBounded(r io.Reader, what string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxDiscoveryAnswerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDiscoveryAnswerBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", what, maxDiscoveryAnswerBytes)
	}
	return raw, nil
}

// refreshFileSD reads every file f's patterns match, in name order, each
// once.
func refreshFileSD(f *FileSDConfig) ([]TargetGroup, error) {
	seen := map[string]bool{}
	var groups []TargetGroup
	for _, pattern := range f.Files {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		for _, path := range matches {
			if seen[path] {
				continue
			}
			seen[path] = true
			found, err := readFileSD(path)
			if err != nil {
				return nil, fmt.Errorf("file_sd file %s: %w", path, err)
			}
			groups = append(groups, found...)
		}
	}
	return groups, nil
}

func readFileSD(path string) ([]TargetGroup, error) {
	file, err := os.Open(path) // #nosec G304 -- a file the discovery source names
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := readBounded(file, "the file")
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return parseGroupsJSON(raw)
	}
	return parseGroupsYAML(raw)
}

// refreshHTTPSD asks h's URL for its target groups.
func refreshHTTPSD(ctx context.Context, h *HTTPSDConfig, refresh string) ([]TargetGroup, error) {
	client, err := fetch.HTTPClient(fetch.TransportSettings{TLS: h.TLS}, false, timeout(h.Timeout))
	if err != nil {
		return nil, fmt.Errorf("http_sd.tls: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// As Prometheus sends it, so the server may tell how fresh to be.
	req.Header.Set("X-Prometheus-Refresh-Interval-Seconds", refresh)
	if h.BearerTokenFile != "" {
		token, err := readToken(h.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("http_sd.bearer_token_file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s answered %s", h.URL, resp.Status)
	}
	raw, err := readBounded(resp.Body, "the answer")
	if err != nil {
		return nil, err
	}
	return parseGroupsJSON(raw)
}

// readToken reads a bearer token file, refusing an empty one.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- a token file the configuration names
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// seconds writes a duration in whole seconds, for the refresh header.
func seconds(d float64) string { return strconv.FormatFloat(d, 'f', -1, 64) }
