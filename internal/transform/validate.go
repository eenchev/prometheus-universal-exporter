package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A pre-script hands its result back through the variable `data`, so a script
// that never produces one silently discards its work: the transform then runs
// against the untouched decoded response and the collector looks like it is
// merely mis-extracting. That is a configuration mistake rather than a runtime
// one, so it is caught before the exporter serves anything.
//
// The check parses each script with Python's own parser, which also surfaces
// syntax errors at startup instead of at the first scrape.

const pythonScriptValidator = `import sys,json,ast

# Only these methods change their receiver. Accepting any method call on data
# would accept a script that merely reads it: data["rates"].items() is a read,
# and treating it as a mutation lets a script that never produces data start,
# which is exactly the mistake this check exists to catch.
MUTATORS={'append','extend','insert','remove','pop','popitem','clear','sort','reverse',
 'add','discard','update','setdefault',
 'difference_update','intersection_update','symmetric_difference_update'}

def roots(node):
    out=[]
    stack=[node]
    while stack:
        n=stack.pop()
        if isinstance(n,ast.Name): out.append(n.id)
        elif isinstance(n,(ast.Subscript,ast.Attribute,ast.Starred)): stack.append(n.value)
        elif isinstance(n,(ast.Tuple,ast.List)): stack.extend(n.elts)
    return out

def produces_data(tree):
    for node in ast.walk(tree):
        targets=[]
        # del data['x'] changes data in place, as an assignment to data['x']
        # does, and (data := ...) assigns data inside an expression.
        if isinstance(node,(ast.Assign,ast.Delete)): targets=node.targets
        elif isinstance(node,(ast.AugAssign,ast.AnnAssign,ast.For,ast.NamedExpr)): targets=[node.target]
        elif isinstance(node,ast.withitem):
            if node.optional_vars is not None: targets=[node.optional_vars]
        elif isinstance(node,ast.Call) and isinstance(node.func,ast.Attribute):
            # data.update(...), data["rates"].append(...) and friends mutate in
            # place; data.items() and data.get(...) only read.
            if node.func.attr in MUTATORS and 'data' in roots(node.func.value): return True
            continue
        for t in targets:
            if 'data' in roots(t): return True
    return False

payload=json.load(sys.stdin)
problems=[]
for item in payload['scripts']:
    label="collector %s %s" % (item['collector'], item['kind'])
    try:
        tree=ast.parse(item['source'], mode='exec')
    except SyntaxError as e:
        problems.append("%s has a Python syntax error on line %s: %s" % (label, e.lineno, e.msg))
        continue
    if item['requires_data'] and not produces_data(tree):
        problems.append("%s must produce its result in a variable named 'data'; assign to data or mutate it in place. Reading it, such as data['x'] or data.items(), does not count: the transform would run against the untouched response" % label)
print(json.dumps({'problems':problems}))`

type pythonScript struct {
	Collector    string `json:"collector"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	RequiresData bool   `json:"requires_data"`
}

// CollectorScripts lists every Python source in the configuration. Only
// pre-scripts are required to produce `data`: a Python transform emits through
// metric(...) instead, so it is checked for syntax only.
func CollectorScripts(c *model.Config) []pythonScript {
	var scripts []pythonScript
	for i := range c.Collectors {
		collector := &c.Collectors[i]
		if source := strings.TrimSpace(collector.Transform.PreScript); source != "" {
			scripts = append(scripts, pythonScript{
				Collector:    collector.Name,
				Kind:         "pre_script",
				Source:       collector.Transform.PreScript,
				RequiresData: true,
			})
		}
		if source := strings.TrimSpace(collector.Transform.Script); source != "" && collector.Transform.Type == "python" {
			scripts = append(scripts, pythonScript{
				Collector: collector.Name,
				Kind:      "python transform script",
				Source:    collector.Transform.Script,
			})
		}
	}
	return scripts
}

// ValidatePythonScripts rejects a configuration whose Python sources cannot
// work. It is a no-op for a configuration containing none, so a deployment that
// uses no Python needs no interpreter present.
func ValidatePythonScripts(pythonPath string, c *model.Config) error {
	problems, err := CheckPythonScripts(pythonPath, c)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid collector Python scripts:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// CheckPythonScripts is ValidatePythonScripts with the faults kept apart: err
// is the interpreter itself failing, and problems are the individual script
// faults, each naming its collector. --dry-run reports the latter one by one.
func CheckPythonScripts(pythonPath string, c *model.Config) ([]string, error) {
	scripts := CollectorScripts(c)
	if len(scripts) == 0 {
		return nil, nil
	}
	if pythonPath == "" {
		pythonPath = "python3"
	}
	input, err := json.Marshal(struct {
		Scripts []pythonScript `json:"scripts"`
	}{Scripts: scripts})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// -B, as the worker runs: the check leaves no bytecode caches behind.
	cmd := exec.CommandContext(ctx, pythonPath, "-I", "-B", "-c", pythonScriptValidator)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("checking collector Python scripts needs a working interpreter at %q: %w: %s", pythonPath, err, strings.TrimSpace(stderr.String()))
	}
	var result struct {
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("checking collector Python scripts: %w", err)
	}
	return result.Problems, nil
}
