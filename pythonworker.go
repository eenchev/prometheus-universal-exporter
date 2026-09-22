package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Python scripts run in long-lived worker interpreters rather than in a fresh
// process per scrape. Starting CPython and importing lxml, PyYAML or dateutil
// takes tens to hundreds of milliseconds; running a typical collector script
// takes well under one. A worker pays the start-up once and then serves
// scrape after scrape.
//
// Isolation. A worker only ever runs one collector's scripts: workers are
// pooled per collector, interpreter, declared libraries, output limit and
// script text, so a script cannot see or disturb another collector's state,
// and a reloaded script gets fresh workers rather than inheriting the old
// one's module state. Each run gets a fresh set of globals. The same sandbox
// as before is installed once, at start-up, before any script runs.
//
// Protocol. Requests go to the worker on file descriptor 3 and answers come
// back on file descriptor 4, one JSON document per line. Neither is stdin or
// stdout, so a script that prints, or reads stdin, cannot corrupt the stream;
// its output is captured per run, as before. The worker's stdin and stdout are
// /dev/null, and stderr is kept, bounded, for error messages.
//
// Time. limits.script_timeout bounds the run of a script, not the start of the
// interpreter: start-up, including importing the collector's declared
// libraries, has its own budget. A script that overruns is not interrupted
// inside the interpreter, which Python cannot do reliably; the worker is
// killed, and the next scrape starts another.
//
// Lifetime. A worker that fails in any way is discarded. A healthy one is
// reused up to pythonWorkerMaxRuns times, so a slow leak in a script or a
// library cannot grow without bound, and at most pythonWorkerMaxIdle are kept
// per pool once a burst of concurrent scrapes has passed. Idle workers are
// stopped after pythonWorkerIdleTimeout. When the exporter exits, a worker
// sees its request pipe close and exits too.

const (
	pythonStartupTimeout    = 10 * time.Second
	pythonWorkerMaxRuns     = 1000
	pythonWorkerMaxIdle     = 4
	pythonWorkerIdleTimeout = 5 * time.Minute
	pythonStderrTail        = 4096
	pythonDefaultMaxOutput  = 1 << 20
)

var (
	errPythonTimeout        = errors.New("python script timed out")
	errPythonOutputTooLarge = errors.New("python output exceeds limit")
)

// pythonLibraryModules are the modules a declared library preloads. Importing
// them at start-up keeps their import time out of script_timeout, and lets a
// library that imports a module the sandbox blocks, such as threading, load
// before the sandbox is installed.
var pythonLibraryModules = map[string][]string{
	"lxml":            {"lxml", "lxml.etree", "lxml.html"},
	"PyYAML":          {"yaml"},
	"yaml":            {"yaml"},
	"python-dateutil": {"dateutil", "dateutil.parser", "dateutil.tz"},
	"dateutil":        {"dateutil", "dateutil.parser", "dateutil.tz"},
}

// pythonSpec says which workers a script may run in.
type pythonSpec struct {
	Path      string
	Collector string
	Modules   []string
	MaxOutput int
	Scripts   string // a digest of the collector's scripts
}

func pythonWorkerSpec(pythonPath string, c *Collector) pythonSpec {
	seen := map[string]bool{}
	var modules []string
	for _, lib := range append(append([]string(nil), c.Transform.Libraries...), c.Transform.RequiredLibs...) {
		for _, module := range pythonLibraryModules[lib] {
			if !seen[module] {
				seen[module] = true
				modules = append(modules, module)
			}
		}
	}
	sort.Strings(modules)
	maxOutput := c.Limits.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = pythonDefaultMaxOutput
	}
	digest := sha256.Sum256([]byte(c.Transform.PreScript + "\x00" + c.Transform.Script))
	return pythonSpec{Path: pythonPath, Collector: c.Name, Modules: modules, MaxOutput: maxOutput, Scripts: hex.EncodeToString(digest[:8])}
}

func (s pythonSpec) key() string {
	return strings.Join([]string{s.Path, s.Collector, strings.Join(s.Modules, ","), strconv.Itoa(s.MaxOutput), s.Scripts}, "\x00")
}

type pythonPool struct {
	mu      sync.Mutex
	idle    map[string][]*pythonWorker
	started atomic.Int64
}

var pythonWorkers = &pythonPool{idle: map[string][]*pythonWorker{}}

// run sends one request to a worker for spec and returns its answer line.
func (p *pythonPool) run(ctx context.Context, spec pythonSpec, payload []byte, timeout time.Duration) ([]byte, error) {
	worker, err := p.acquire(ctx, spec)
	if err != nil {
		return nil, err
	}
	line, err := worker.call(ctx, payload, timeout)
	if err != nil {
		worker.stop()
		return nil, err
	}
	p.release(spec, worker)
	return line, nil
}

func (p *pythonPool) acquire(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
	key := spec.key()
	p.mu.Lock()
	p.reapLocked(time.Now())
	var worker *pythonWorker
	if idle := p.idle[key]; len(idle) > 0 {
		worker = idle[len(idle)-1]
		p.idle[key] = idle[:len(idle)-1]
	}
	p.mu.Unlock()
	if worker != nil {
		return worker, nil
	}
	p.started.Add(1)
	return startPythonWorker(ctx, spec)
}

func (p *pythonPool) release(spec pythonSpec, worker *pythonWorker) {
	worker.runs++
	if worker.runs >= pythonWorkerMaxRuns {
		worker.stop()
		return
	}
	key := spec.key()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle[key]) >= pythonWorkerMaxIdle {
		worker.stop()
		return
	}
	worker.idleSince = time.Now()
	p.idle[key] = append(p.idle[key], worker)
}

// reapLocked stops workers idle for longer than the idle timeout, which also
// retires the workers of a script a reload replaced.
func (p *pythonPool) reapLocked(now time.Time) {
	for key, workers := range p.idle {
		kept := workers[:0]
		for _, worker := range workers {
			if now.Sub(worker.idleSince) > pythonWorkerIdleTimeout {
				worker.stop()
				continue
			}
			kept = append(kept, worker)
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
}

type pythonLine struct {
	data []byte
	err  error
}

type pythonWorker struct {
	cmd       *exec.Cmd
	requests  *os.File
	lines     chan pythonLine
	stderr    *tailBuffer
	runs      int
	idleSince time.Time
	stopOnce  sync.Once
}

func startPythonWorker(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
	modules, err := json.Marshal(append([]string{}, spec.Modules...))
	if err != nil {
		return nil, err
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	answerRead, answerWrite, err := os.Pipe()
	if err != nil {
		closeFiles(requestRead, requestWrite)
		return nil, err
	}
	// The worker outlives the scrape that starts it, so it must not be tied to
	// that scrape's context; stop() ends it.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Path, "-I", "-c", pythonWorkerLauncher, string(modules)) // #nosec G204 -- the interpreter is the operator's --python.path
	cmd.ExtraFiles = []*os.File{requestRead, answerWrite}                                                                // descriptors 3 and 4
	stderr := &tailBuffer{max: pythonStderrTail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		closeFiles(requestRead, requestWrite, answerRead, answerWrite)
		return nil, err
	}
	// The child holds its own copies of these ends.
	closeFiles(requestRead, answerWrite)

	worker := &pythonWorker{cmd: cmd, requests: requestWrite, lines: make(chan pythonLine, 1), stderr: stderr}
	go worker.readAnswers(answerRead, spec.MaxOutput)
	go func() { _ = cmd.Wait() }()

	timer := time.NewTimer(pythonStartupTimeout)
	defer timer.Stop()
	select {
	case line, ok := <-worker.lines:
		if !ok || line.err != nil || !strings.Contains(string(line.data), `"ready": true`) {
			worker.stop()
			return nil, fmt.Errorf("the interpreter did not start: %s", worker.describe(line.err))
		}
		return worker, nil
	case <-timer.C:
		worker.stop()
		return nil, fmt.Errorf("the interpreter did not start within %s: %s", pythonStartupTimeout, worker.describe(nil))
	case <-ctx.Done():
		worker.stop()
		return nil, ctx.Err()
	}
}

// readAnswers turns the answer stream into lines until the worker exits or
// writes a line longer than the output limit.
func (w *pythonWorker) readAnswers(answers *os.File, maxOutput int) {
	defer close(w.lines)
	defer closeFiles(answers)
	scanner := bufio.NewScanner(answers)
	scanner.Buffer(make([]byte, 0, min(4096, maxOutput+1)), maxOutput+1)
	for scanner.Scan() {
		w.lines <- pythonLine{data: append([]byte(nil), scanner.Bytes()...)}
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		w.lines <- pythonLine{err: errPythonOutputTooLarge}
	}
}

func (w *pythonWorker) call(ctx context.Context, payload []byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	_ = w.requests.SetWriteDeadline(deadline)
	if _, err := w.requests.Write(append(payload, '\n')); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, errPythonTimeout
		}
		return nil, errors.New(w.describe(err))
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case line, ok := <-w.lines:
		if !ok {
			return nil, errors.New(w.describe(nil))
		}
		if line.err != nil {
			return nil, line.err
		}
		return line.data, nil
	case <-timer.C:
		return nil, errPythonTimeout
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errPythonTimeout
		}
		return nil, ctx.Err()
	}
}

// describe explains why a worker failed, with the end of what it wrote to
// stderr, which is where CPython reports a crash or a failed start.
func (w *pythonWorker) describe(err error) string {
	message := "the interpreter exited"
	if err != nil {
		message = err.Error()
	}
	if tail := strings.TrimSpace(w.stderr.String()); tail != "" {
		message += ": " + tail
	}
	return message
}

func (w *pythonWorker) stop() {
	w.stopOnce.Do(func() {
		closeFiles(w.requests)
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
	})
}

// closeFiles closes pipe ends whose close cannot usefully fail: each is
// either already handed to the child or being abandoned with the worker.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// pythonWorkerLauncher is the worker. It opens its request and answer
// descriptors and preloads the declared libraries, then installs the sandbox
// — blocked modules, disabled process, file and descriptor functions — and
// only then answers that it is ready. Each request runs the script in fresh
// globals holding the same names scripts have always had, with stdout and
// stderr captured, and answers with the metrics, the data or the error.
const pythonWorkerLauncher = `import sys,json,builtins,contextlib,io,os,traceback
requests=os.fdopen(3,'r',encoding='utf-8')
answers=os.fdopen(4,'w',encoding='utf-8')
for _module in json.loads(sys.argv[1]) or []:
    try: __import__(_module)
    except Exception: pass
blocked={'socket','subprocess','ctypes','multiprocessing','threading','_ctypes','pathlib','shutil','tempfile'}
real_import=builtins.__import__
def guarded_import(name,*a,**kw):
    if name.split('.')[0] in blocked or name in {'urllib.request','urllib.error','urllib.robotparser'}: raise ImportError('module disabled by exporter')
    return real_import(name,*a,**kw)
builtins.__import__=guarded_import
def denied(*a,**kw): raise RuntimeError('operation disabled by exporter')
for _name in ('system','popen','spawnl','spawnlp','spawnv','spawnvp','execv','execve','execvp','fork','open','listdir','scandir','walk','remove','unlink','rename','replace','mkdir','makedirs','rmdir','fdopen','read','write','dup','dup2','close','kill','killpg'):
    if hasattr(os,_name): setattr(os,_name,denied)
builtins.open=denied; io.open=denied
class Response:
    def __init__(self,x): self.status_code=x['status_code']; self.headers=x['headers']; self.body=x['body']; self.text=x['text']
    def json(self): return json.loads(self.text)
    def yaml(self):
        try:
            import yaml
        except ImportError: raise RuntimeError('yaml library is not available')
        return yaml.safe_load(self.text)
def answer(document):
    answers.write(json.dumps(document)+'\n'); answers.flush()
answer({'ok': True, 'ready': True})
while True:
    line=requests.readline()
    if not line: break
    try:
        p=json.loads(line)
        metrics=[]
        def metric(name,type='gauge',value=0,labels=None,help=None,timestamp=None,_metrics=metrics):
            if not isinstance(name,str): raise ValueError('metric name must be a string')
            if labels is None: labels={}
            _metrics.append({'name':name,'type':type,'value':value,'labels':labels,'help':help or '','timestamp':timestamp})
        def fail(message): raise RuntimeError(str(message))
        scope={'__builtins__':builtins,'__name__':'__collector__','sys':sys,'json':json,'builtins':builtins,'contextlib':contextlib,'io':io,'os':os,
               'metric':metric,'fail':fail,'Response':Response,'response':Response(p['response']),'target':p['target'],'collector':p['collector'],'data':p['data'],'metrics':metrics}
        sink=io.StringIO()
        with contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
            exec(compile(p['script'],'<collector-python>','exec'),scope,scope)
        result={'ok':True,'log':sink.getvalue()}
        if p.get('mode')=='data': result['data']=scope.get('data')
        else: result['metrics']=metrics
        answer(result)
    except BaseException:
        answer({'ok': False, 'error': traceback.format_exc(limit=5)})`
