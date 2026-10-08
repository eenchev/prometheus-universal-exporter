package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// How a script's run was handed to a worker and its answer read before the
// hand-over was made cheaper (pythonrequest.go, pythonanswer.go and the
// worker's answer and metric): the request written by json.Marshal, the
// answer read by encoding/json into maps that were then read into series,
// and the worker as it was, which walked every answer through wire. They
// are kept here as the oracle the tests of the hand-over compare it with
// (pythonrequest_test.go, pythonanswer_test.go, pythonhandover_test.go).

// oraclePythonRequest is pythonRequest as it was.
func oraclePythonRequest(mode, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) ([]byte, error) {
	input := pythonInput{Mode: mode, Script: script, Target: r.Target, Collector: c.Name, Response: pythonResponse{StatusCode: r.Status(), Headers: r.Headers, Body: string(r.Body)}}
	if data := pythonScriptData(d); dataIsBody(data, input.Response.Body) {
		input.DataIsBody = true
	} else {
		input.Data = withNonFiniteMarkers(data)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(payload, nonFiniteJSONMarker) {
		payload = []byte(nonFiniteJSON.Replace(string(payload)))
	}
	return payload, nil
}

// oraclePythonAnswer is how pythonResult read a worker's answer line: the
// output, or the error of a line that is no answer or of a script that
// failed.
func oraclePythonAnswer(what string, line []byte) (*pythonOutput, error) {
	var out pythonOutput
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("python %s output: %w", what, err)
	}
	if !out.OK {
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	return &out, nil
}

// oraclePythonSeries is how executePython made series of an answer's
// metrics.
func oraclePythonSeries(ctx context.Context, out *pythonOutput) (*model.MetricSet, error) {
	if err := takeSeriesN(ctx, len(out.Metrics)); err != nil {
		return nil, err
	}
	set := &model.MetricSet{Metrics: make([]model.Metric, 0, len(out.Metrics))}
	for i, raw := range out.Metrics {
		emitted, err := pythonMetricFrom(i, raw)
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		metric, err := emitted.metric()
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		set.Metrics = append(set.Metrics, metric)
	}
	return set, nil
}

// oraclePythonData is how executePythonPreScript read what a pre-script
// left in data.
func oraclePythonData(out *pythonOutput) any {
	return pythonFloats(model.Normalize(out.Data))
}

// oraclePythonPool is a pool whose workers run the worker as it was.
func oraclePythonPool() *PythonPool {
	pool := newPythonPool()
	pool.start = func(ctx context.Context, spec pythonSpec) (*pythonWorker, error) {
		return startPythonWorkerRunning(ctx, spec, oraclePythonLauncher)
	}
	return pool
}

// oracleTimeout is the script_timeout of the runs that compare the two.
const oracleTimeout = time.Minute

// oraclePythonLauncher is pythonWorkerLauncher as it was.
const oraclePythonLauncher = `import sys,json,builtins,contextlib,io,os,traceback,decimal,linecache,tokenize
requests=os.fdopen(3,'r',encoding='utf-8')
answers=os.fdopen(4,'w',encoding='utf-8')
def watch_parent():
    # A worker busy in a script does not see its request pipe close when the
    # exporter is gone, and an exporter that was killed stops nobody. So a
    # thread looks once a second whether the worker's parent is still the
    # process that started it, and ends the worker when it is not: an orphan
    # is given another parent. It is started here, before the memory limit
    # and the sandbox, with what it calls bound now, so neither a limit too
    # small for a thread nor a script that replaces os.getppid stops it, and
    # the launcher goes on only once the thread runs: a thread still starting
    # when the limit is installed could fail of it before its first line.
    # Its stack is small, since it counts against that limit, and its
    # modules are imported in here, so the launcher's own names stay as they
    # were.
    import _thread,time
    parent,getppid,leave,sleep=os.getppid(),os.getppid,os._exit,time.sleep
    running=_thread.allocate_lock()
    running.acquire()
    def watch():
        running.release()
        while True:
            # A script that has used all of limits.max_script_memory leaves
            # none for the number getppid answers with. The watch outlives
            # that, and whatever else is raised in here, and looks again a
            # second later: it must not end while the worker lives. Nothing
            # is named after except, so nothing a script can replace.
            try:
                sleep(1)
                if getppid()!=parent: leave(0)
            except: pass
    try: _thread.stack_size(262144)
    except Exception: pass
    try:
        _thread.start_new_thread(watch,())
        running.acquire(True,5)
    except Exception: pass
    try: _thread.stack_size(0)
    except Exception: pass
watch_parent()
del watch_parent
for _module in json.loads(sys.argv[1]) or []:
    try: __import__(_module)
    except Exception: pass
max_memory=int(sys.argv[2]) if len(sys.argv)>2 else 0
if max_memory>0:
    # limits.max_script_memory: the worker's whole address space, the
    # interpreter and its preloaded libraries included, set after they have
    # loaded so a limit too small for them fails a run, not the start.
    import resource
    resource.setrlimit(resource.RLIMIT_AS,(max_memory,max_memory))
# zoneinfo reads its search path through sysconfig, which imports threading
# from Python 3.12 on. threading is blocked, so a script's import zoneinfo
# would fail; it is imported here, before the sandbox, and sysconfig's
# reference to threading is dropped below once the sandbox is in place.
try: import zoneinfo
except Exception: pass
blocked={'socket','_socket','ssl','_ssl','subprocess','_posixsubprocess','ctypes','_ctypes','multiprocessing','_multiprocessing','threading','mmap','pty','pathlib','shutil','tempfile'}
script_blocked={'importlib','posix','nt','_io','_thread','select','selectors','fcntl','termios'}
import _io
real_import=builtins.__import__
def guarded_import(name,globals=None,*a,**kw):
    top=name.split('.')[0]
    if top in blocked or name in {'urllib.request','urllib.error','urllib.robotparser'}: raise ImportError('module disabled by exporter')
    if top in script_blocked and (globals or {}).get('__name__')=='__collector__': raise ImportError('module disabled by exporter')
    return real_import(name,globals,*a,**kw)
builtins.__import__=guarded_import
for _name in [n for n in sys.modules if n.split('.')[0] in blocked or n in {'posix','nt'}]: del sys.modules[_name]
# sysconfig needs threading only for the lock it made when it loaded; left
# as its attribute, the blocked module would be one attribute away.
if 'threading' in vars(sys.modules.get('sysconfig',sys)): del sys.modules['sysconfig'].threading
def denied(*a,**kw): raise RuntimeError('operation disabled by exporter')
for _name in ('system','popen','spawnl','spawnle','spawnlp','spawnlpe','spawnv','spawnve','spawnvp','spawnvpe','posix_spawn','posix_spawnp','execl','execle','execlp','execlpe','execv','execve','execvp','execvpe','fork','forkpty','openpty','pipe','pipe2','open','listdir','scandir','walk','fwalk','remove','unlink','rename','replace','mkdir','makedirs','rmdir','removedirs','link','symlink','truncate','ftruncate','chmod','chown','lchown','mkfifo','mknod','fdopen','read','readv','pread','write','writev','pwrite','sendfile','dup','dup2','close','closerange','kill','killpg'):
    if hasattr(os,_name): setattr(os,_name,denied)
def tz_roots():
    # Time zone data may be read, and nothing else: the system's (zoneinfo's
    # TZPATH, where dateutil.tz looks too), dateutil's bundled copy and the
    # tzdata package's, when they are there. Resolved now, before the
    # sandbox, so a symlink out of them leads nowhere.
    roots=['/usr/share/zoneinfo','/usr/lib/zoneinfo','/usr/share/lib/zoneinfo','/etc/zoneinfo']
    try:
        import zoneinfo
        roots+=list(zoneinfo.TZPATH)
    except Exception: pass
    for package in ('dateutil.zoneinfo','tzdata'):
        try: roots.append(os.path.dirname(__import__(package,fromlist=['_']).__file__))
        except Exception: pass
    return tuple(sorted({os.path.realpath(r).rstrip(os.sep)+os.sep for r in roots if r}))
tz_readable_roots=tz_roots()
tz_readable_files={os.path.realpath('/etc/localtime')}
def tz_readable(file,mode):
    if mode not in ('r','rb','rt','br','tr') or not isinstance(file,(str,bytes,os.PathLike)): return False
    try:
        path=os.fsdecode(os.fspath(file))
        real=os.path.realpath(path)
    except Exception: return False
    return real in tz_readable_files or real.startswith(tz_readable_roots)
def tz_only(real):
    # open(), io.open() and io.FileIO read time zone data and nothing else.
    def opened(file,mode='r',*a,**kw):
        if tz_readable(file,mode): return real(file,mode,*a,**kw)
        raise RuntimeError('operation disabled by exporter')
    return opened
def code_only(open_code):
    # The importer reads a module's source and bytecode through _io.open.
    def opened(file,mode='r',*a,**kw):
        if mode=='rb' and isinstance(file,str) and file.endswith(('.py','.pyc')): return open_code(file,mode,*a,**kw)
        if tz_readable(file,mode): return open_code(file,mode,*a,**kw)
        raise RuntimeError('operation disabled by exporter')
    return opened
# As the worker does (pythonworker.go), so that a traceback reads a
# library's source on Python 3.13 too, and a script reads nothing else.
tokenize._builtin_open=code_only(io.open)
builtins.open=tz_only(io.open); io.FileIO=tz_only(_io.FileIO); _io.open=code_only(_io.open); io.open=builtins.open; _io.FileIO=io.FileIO
del _io, _name, code_only, tz_only, tz_roots
class Response:
    # The body is sent once: text is the same string.
    def __init__(self,x): self.status_code=x['status_code']; self.headers=x['headers']; self.body=self.text=x['body']
    def header(self,name,default=None):
        # One header's values joined by ", ", as jq's $headers has them, the
        # name in any case; default when the response has none.
        values=[v for k,vs in (self.headers or {}).items() if k.lower()==name.lower() for v in vs]
        return ', '.join(values) if values else default
    def json(self): return json.loads(self.text)
    def yaml(self):
        try:
            import yaml
        except ImportError: raise RuntimeError('yaml library is not available')
        return yaml.safe_load(self.text)
def label_text(name,v):
    # A label value as text, the way jq labels are written (transform/labeltext.go):
    # numbers as JSON writes them, booleans as true and false, None as no label.
    if v is None or isinstance(v,str): return v
    if isinstance(v,bool): return 'true' if v else 'false'
    if isinstance(v,int): return str(v)
    if isinstance(v,float):
        if v!=v: return 'NaN'
        if v in (float('inf'),float('-inf')): return '+Inf' if v>0 else '-Inf'
        if v==0: return '0'
        if 1e-6<=abs(v)<1e21:
            text=format(decimal.Decimal(repr(v)),'f')
            return text.rstrip('0').rstrip('.') if '.' in text else text
        return repr(v)
    if isinstance(v,(dict,list,tuple,set)): raise ValueError('label %r is a %s, not a single value; pass one value, or join them with ",".join(...)'%(name,type(v).__name__))
    return str(v)
def wire(v):
    # NaN and the infinities have no JSON form: each goes as a marker the
    # exporter reads back as the float (transform/python.go).
    if isinstance(v,float) and (v!=v or v in (float('inf'),float('-inf'))):
        return '\x00pue-nonfinite:'+('NaN' if v!=v else '+Inf' if v>0 else '-Inf')+'\x00'
    if isinstance(v,dict): return {k:wire(x) for k,x in v.items()}
    if isinstance(v,(list,tuple)): return [wire(x) for x in v]
    return v
def metric_number(name,what,v):
    # A value or timestamp as a number: a bool as 1 or 0, a numeric string
    # as the number it reads as, and anything else refused naming the metric.
    if isinstance(v,bool): return 1.0 if v else 0.0
    if isinstance(v,(int,float)): return float(v)
    if isinstance(v,str):
        try: return float(v.strip())
        except ValueError: pass
    raise ValueError('metric %r %s %r is not a number'%(name,what,v))
def script_error(e):
    # The error as a traceback of the script's own frames, innermost last:
    # the worker's frames (this launcher, run as "<string>") are dropped, and
    # of the rest the five innermost are kept, the failing line among them.
    # Chained exceptions ("During handling of ...") are trimmed alike.
    shown=traceback.TracebackException.from_exception(e)
    te,seen=shown,set()
    while te is not None and id(te) not in seen:
        seen.add(id(te))
        te.stack=traceback.StackSummary.from_list([f for f in te.stack if f.filename!='<string>'][-5:])
        te=te.__cause__ or te.__context__
    return ''.join(shown.format())
def answer(document):
    answers.write(json.dumps(wire(document),allow_nan=False)+'\n'); answers.flush()
answer({'ok': True, 'ready': True})
while True:
    line=requests.readline()
    if not line: break
    try:
        p=json.loads(line)
        # The request's text is not kept while the script runs, and data
        # that is the body is the body's own string, not a copy.
        line=None
        if p.get('data_is_body'): p['data']=p['response']['body']
        # The request is read: the script's time, limits.script_timeout,
        # starts when the exporter reads this line.
        answer({'started': True})
        metrics=[]
        def metric(name,type='gauge',value=0,labels=None,help=None,timestamp=None,_metrics=metrics):
            if not isinstance(name,str): raise ValueError('metric name %r is not a string'%(name,))
            if type is None: type='gauge'
            if not isinstance(type,str): raise ValueError('metric %r type %r is not a string; give "gauge", "counter" or "untyped"'%(name,type))
            if help is not None and not isinstance(help,str): raise ValueError('metric %r help %r is not a string'%(name,help))
            if labels is None: labels={}
            if not isinstance(labels,dict): raise ValueError('metric %r labels must be a mapping of label names to values, not a %s'%(name,labels.__class__.__name__))
            labels={str(k):t for k,t in ((k,label_text(k,v)) for k,v in labels.items()) if t is not None}
            value=metric_number(name,'value',value)
            if timestamp is not None:
                timestamp=metric_number(name,'timestamp',timestamp)
                if timestamp!=timestamp or timestamp in (float('inf'),float('-inf')): raise ValueError('metric %r timestamp is not a number of milliseconds'%name)
                timestamp=int(timestamp)
            _metrics.append({'name':name,'type':type,'value':value,'labels':labels,'help':help or '','timestamp':timestamp})
        def fail(message): raise RuntimeError(str(message))
        scope={'__builtins__':builtins,'__name__':'__collector__','sys':sys,'json':json,'builtins':builtins,'contextlib':contextlib,'io':io,'os':os,
               'metric':metric,'fail':fail,'Response':Response,'response':Response(p['response']),'target':p['target'],'collector':p['collector'],'data':p['data'],'metrics':metrics}
        sink=io.StringIO()
        # The script's source, for its lines in a traceback.
        linecache.cache['<collector-python>']=(len(p['script']),None,p['script'].splitlines(True),'<collector-python>')
        with contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
            exec(compile(p['script'],'<collector-python>','exec'),scope,scope)
        log=sink.getvalue()
        if len(log)>4096: log=log[:4096]+'... (%d more characters)'%(len(log)-4096)
        result={'ok':True,'log':log}
        if p.get('mode')=='data': result['data']=scope.get('data')
        else: result['metrics']=metrics
        answer(result)
    except MemoryError as e:
        answer({'ok': False, 'error': 'MemoryError: the script ran out of memory under limits.max_script_memory (%d bytes)'%max_memory if max_memory>0 else script_error(e)})
    except BaseException as e:
        answer({'ok': False, 'error': script_error(e)})`
