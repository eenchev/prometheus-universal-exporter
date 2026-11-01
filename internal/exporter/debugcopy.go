package exporter

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Edit a copy makes the playground a copy of the configured collector the
// page shows. A copy of one that names a file of the server's — a key of
// its request ending in _file or _files, such as tls.ca_file or
// password_file, or a login, which reads its secrets from files — would be
// refused until those lines were taken out by hand (refusedOnDebugPage),
// and the page shows their paths <redacted> besides. So the copy leaves
// them out, each line replaced by a comment that says what was dropped and
// why, and a mapping left with nothing, tls: with its ca_file alone, goes
// with it, so that the copy loads. With --web.debug.dangerous the
// playground may read files, and keeps a login, but the paths are still
// withheld: they are dropped all the same, the comment saying to write the
// path in. Python is left in: a script dropped would leave a collector that
// does not load, and the refusal says what to do; the note above the
// editor says so beforehand.

// copiedCollectorYAML is x as Edit a copy puts it in the playground
// (collectorYAML), and the note the page shows above it, "" when nothing
// was left out and nothing will be refused.
func copiedCollectorYAML(x *model.Collector, dangerous bool) (string, string, error) {
	drops := &copyDrops{dangerous: dangerous}
	text, err := collectorYAML(x, drops)
	if err != nil {
		return "", "", err
	}
	var notes []string
	if len(drops.dropped) > 0 {
		names := "request." + strings.Join(drops.dropped, ", request.")
		if dangerous {
			notes = append(notes, "The copy leaves out "+names+": the page withholds the paths they name, so write each in place of its comment.")
		} else {
			notes = append(notes, "The copy leaves out "+names+": the playground reads no file of the server's, and the comments in their place say so. A target probed through the collector it was copied from is fetched with them.")
		}
	}
	if runsPython(x) && !dangerous {
		notes = append(notes, "It runs Python, which the playground never runs: a run of the copy is refused until transform.pre_script, or the python transform, is taken out.")
	}
	return text, strings.Join(notes, " "), nil
}

// droppedKey stands in, as a key with no value, for what the copy leaves
// out while it is encoded, and is then replaced with the comments that say
// so.
const droppedKey = "pue-dropped-"

// copyDrops leaves out of a copy what the playground refuses, recording
// what it left out.
type copyDrops struct {
	dangerous bool
	// dropped are the keys left out, by their paths under request, in the
	// order of the block.
	dropped []string
	// comments are, by droppedKey<i>, the comments that stand in its place.
	comments [][]string
}

// dropFrom leaves out of the collector's mapping n what the playground
// refuses of its request block.
func (d *copyDrops) dropFrom(n *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "request" && n.Content[i+1].Kind == yaml.MappingNode {
			d.dropped, _ = d.dropIn(n.Content[i+1], "")
		}
	}
}

// refused says whether the playground refuses the key at path under
// request (serverFileKeys): login, unless dangerous, and every key ending
// in _file or _files, but max_files, a count.
func (d *copyDrops) refused(path, key string) bool {
	if path == "login" {
		return !d.dangerous
	}
	return key != "max_files" && (strings.HasSuffix(key, "_file") || strings.HasSuffix(key, "_files"))
}

// why is the comment that stands in for the key at path under request,
// named as written where it stands.
func (d *copyDrops) why(path, written string) string {
	switch {
	case path == "login":
		return written + " dropped: a login reads its secrets from the server's files, which the playground reads none of (--web.debug.dangerous lets it)"
	case d.dangerous:
		return written + " dropped: the page does not show the path it names; write it here"
	}
	return written + " dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)"
}

// dropIn leaves out of the mapping n, at path under request ("" for the
// block itself), the keys refused, and those of the mappings it holds, each
// replaced by a droppedKey: a mapping all of whose keys are left out is
// left out whole, its droppedKey saying what each of them was. It returns
// the paths left out, under path, and whether nothing of n is left.
func (d *copyDrops) dropIn(n *yaml.Node, path string) ([]string, bool) {
	var dropped []string
	kept, left := make([]*yaml.Node, 0, len(n.Content)), 0
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		at := key.Value
		if path != "" {
			at = path + "." + key.Value
		}
		switch {
		case d.refused(at, key.Value):
			dropped = append(dropped, at)
			kept = append(kept, d.stand([]string{d.why(at, key.Value)})...)
		case value.Kind == yaml.MappingNode && len(value.Content) > 0:
			inner, emptied := d.dropIn(value, at)
			dropped = append(dropped, inner...)
			if !emptied {
				kept, left = append(kept, key, value), left+1
				continue
			}
			// Each named as it is under n, where the droppedKey stands.
			comments := make([]string, len(inner))
			for j, p := range inner {
				comments[j] = d.why(p, strings.TrimPrefix(p, path+"."))
			}
			kept = append(kept, d.stand(comments)...)
		default:
			kept, left = append(kept, key, value), left+1
		}
	}
	n.Content = kept
	return dropped, left == 0 && len(dropped) > 0
}

// stand is a droppedKey<i>, with no value, for comments.
func (d *copyDrops) stand(comments []string) []*yaml.Node {
	d.comments = append(d.comments, comments)
	return []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: droppedKey + strconv.Itoa(len(d.comments)-1)},
		{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"},
	}
}

// Replace writes, in place of each line of the encoded YAML that is a
// droppedKey<i>, its comments, indented as the key was.
func (d *copyDrops) Replace(text string) string {
	if len(d.comments) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		var i int
		if _, err := fmt.Sscanf(trimmed, droppedKey+"%d: null", &i); err != nil || trimmed != droppedKey+strconv.Itoa(i)+": null" || i < 0 || i >= len(d.comments) {
			out = append(out, line)
			continue
		}
		for _, comment := range d.comments[i] {
			out = append(out, line[:len(line)-len(trimmed)]+"# "+comment)
		}
	}
	return strings.Join(out, "\n")
}
