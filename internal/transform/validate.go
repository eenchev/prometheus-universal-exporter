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
 'difference_update','intersection_update','symmetric_difference_update',
 '__setitem__','__delitem__','__ior__','__iadd__'}

def roots(node):
    out=[]
    stack=[node]
    while stack:
        n=stack.pop()
        if isinstance(n,ast.Name): out.append(n.id)
        elif isinstance(n,(ast.Subscript,ast.Attribute,ast.Starred)): stack.append(n.value)
        elif isinstance(n,(ast.Tuple,ast.List)): stack.extend(n.elts)
    return out

# A name bound to a part of data is data by another name: rows = data["rows"],
# for row in data["rows"], for name, value in data.items(). What is changed
# through it is changed in data, as in the commonest pre-script there is,
#     for row in data["rows"]:
#         row["cpu"] = float(row["cpu"].rstrip("%"))
# which mutates nothing rooted at the name data. PARTS are the methods that
# hand out parts of their receiver rather than a copy of it, and WALKS the
# builtins that hand out the items of what they are given, which a loop may
# stand between itself and data.
PARTS={'get','setdefault','pop','values','items'}
WALKS={'enumerate','reversed','sorted','list','tuple','iter','zip','filter'}

def part_of(node,names,walking):
    if isinstance(node,ast.Name): return node.id in names
    if isinstance(node,(ast.Subscript,ast.Attribute,ast.Starred)): return part_of(node.value,names,walking)
    if isinstance(node,(ast.Tuple,ast.List)): return any(part_of(e,names,walking) for e in node.elts)
    if isinstance(node,ast.BoolOp): return any(part_of(v,names,walking) for v in node.values)
    if isinstance(node,ast.IfExp): return part_of(node.body,names,walking) or part_of(node.orelse,names,walking)
    if isinstance(node,ast.Call):
        if isinstance(node.func,ast.Attribute) and node.func.attr in PARTS: return part_of(node.func.value,names,walking)
        if walking and isinstance(node.func,ast.Name) and node.func.id in WALKS: return any(part_of(a,names,walking) for a in node.args)
    return False

def bound(target):
    return [n.id for n in ast.walk(target) if isinstance(n,ast.Name) and isinstance(n.ctx,ast.Store)]

def parts_of_data(tree):
    names={'data'}
    while True:
        before=len(names)
        for node in ast.walk(tree):
            if isinstance(node,ast.Assign) and part_of(node.value,names,False):
                for t in node.targets: names.update(bound(t))
            elif isinstance(node,(ast.AnnAssign,ast.NamedExpr)) and node.value is not None and part_of(node.value,names,False):
                names.update(bound(node.target))
            elif isinstance(node,(ast.For,ast.AsyncFor,ast.comprehension)) and part_of(node.iter,names,True):
                names.update(bound(node.target))
        if len(names)==before: return names

def produces_data(tree):
    names=parts_of_data(tree)
    # A function of the script's own that is handed data, or a part of it,
    # may change what it is handed: def clean(d): ...; clean(data).
    own={n.name for n in ast.walk(tree) if isinstance(n,(ast.FunctionDef,ast.AsyncFunctionDef))}
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
            if node.func.attr in MUTATORS and names.intersection(roots(node.func.value)): return True
            continue
        elif isinstance(node,ast.Call):
            called=isinstance(node.func,ast.Lambda) or isinstance(node.func,ast.Name) and node.func.id in own
            if called and any(part_of(a,names,False) for a in list(node.args)+[k.value for k in node.keywords]): return True
            continue
        for t in targets:
            if 'data' in roots(t): return True
            # Through another name only what it names is changed: row["x"] = 1
            # changes data, row = 1 names something else.
            for part in ast.walk(t):
                if isinstance(part,(ast.Subscript,ast.Attribute)) and isinstance(part.ctx,(ast.Store,ast.Del)) and names.intersection(roots(part)): return True
    return False

payload=json.load(sys.stdin)
problems=[]
for item in payload['scripts']:
    label="collector %s %s" % (item['collector'], item['kind'])
    try:
        tree=ast.parse(item['source'], mode='exec')
    except SyntaxError as e:
        problems.append({'collector':item['collector'],'message':"%s has a Python syntax error on line %s: %s" % (label, e.lineno, e.msg)})
        continue
    if item['requires_data'] and not produces_data(tree):
        problems.append({'collector':item['collector'],'message':"%s must produce its result in a variable named 'data'; assign to data or mutate it in place. Reading it, such as data['x'] or data.items(), does not count: the transform would run against the untouched response" % label})
print(json.dumps({'problems':problems}))`

// PythonScriptProblem is one fault the check found in a collector's Python:
// the message, which names the collector, and the collector's name on its
// own, for a caller that has to say where the collector is defined.
type PythonScriptProblem struct {
	Collector string `json:"collector"`
	Message   string `json:"message"`
}

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
		messages := make([]string, len(problems))
		for i, problem := range problems {
			messages[i] = problem.Message
		}
		return fmt.Errorf("invalid collector Python scripts:\n  %s", strings.Join(messages, "\n  "))
	}
	return nil
}

// CheckPythonScripts is ValidatePythonScripts with the faults kept apart: err
// is the interpreter itself failing, and problems are the individual script
// faults, in the order of the collectors, each naming its collector in its
// message and beside it. --dry-run reports the latter one by one. The
// interpreter is run once, whatever it finds.
func CheckPythonScripts(pythonPath string, c *model.Config) ([]PythonScriptProblem, error) {
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
		// An interpreter that is not there says nothing on stderr: the
		// error alone is the message then, not one ending in ": ".
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("checking collector Python scripts needs a working interpreter at %q: %w: %s", pythonPath, err, detail)
		}
		return nil, fmt.Errorf("checking collector Python scripts needs a working interpreter at %q: %w", pythonPath, err)
	}
	var result struct {
		Problems []PythonScriptProblem `json:"problems"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("checking collector Python scripts: %w", err)
	}
	return result.Problems, nil
}
