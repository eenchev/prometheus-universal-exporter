package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type pythonInput struct { Script string `json:"script"`; Data any `json:"data"`; Response pythonResponse `json:"response"`; Target string `json:"target"`; Collector string `json:"collector"` }
type pythonResponse struct { StatusCode int `json:"status_code"`; Headers map[string][]string `json:"headers"`; Body string `json:"body"`; Text string `json:"text"` }

// The launcher deliberately receives the script as data. It blocks imports and
// process/network primitives before executing collector code, while keeping the
// normal Python standard library available for parsing and arithmetic.
const pythonLauncher = `import sys,json,builtins,contextlib,io,os
p=json.load(sys.stdin)
blocked={'socket','subprocess','ctypes','multiprocessing','threading','_ctypes','pathlib','shutil','tempfile'}
real_import=builtins.__import__
def guarded_import(name,*a,**kw):
    if name.split('.')[0] in blocked or name in {'urllib.request','urllib.error','urllib.robotparser'}: raise ImportError('module disabled by exporter')
    return real_import(name,*a,**kw)
builtins.__import__=guarded_import
def denied(*a,**kw): raise RuntimeError('operation disabled by exporter')
os.system=denied; os.popen=denied; os.spawnl=denied; os.spawnlp=denied; os.spawnv=denied; os.spawnvp=denied; os.execv=denied; os.execve=denied; os.execvp=denied; os.fork=denied; os.open=denied; os.listdir=denied; os.scandir=denied; os.walk=denied; os.remove=denied; os.unlink=denied; os.rename=denied; os.replace=denied; os.mkdir=denied; os.makedirs=denied; os.rmdir=denied
builtins.open=denied; io.open=denied
metrics=[]
def metric(name,type='gauge',value=0,labels=None,help=None,timestamp=None):
    if not isinstance(name,str): raise ValueError('metric name must be a string')
    if labels is None: labels={}
    metrics.append({'name':name,'type':type,'value':value,'labels':labels,'help':help or '', 'timestamp':timestamp})
def fail(message): raise RuntimeError(str(message))
class Response:
    def __init__(self,x): self.status_code=x['status_code']; self.headers=x['headers']; self.body=x['body']; self.text=x['text']
    def json(self): return json.loads(self.text)
    def yaml(self):
        try:
            import yaml
        except ImportError: raise RuntimeError('yaml library is not available')
        return yaml.safe_load(self.text)
response=Response(p['response']); target=p['target']; collector=p['collector']; data=p['data']
sink=io.StringIO()
with contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
    exec(compile(p['script'],'<collector-python>','exec'),globals(),globals())
print(json.dumps({'metrics':metrics,'log':sink.getvalue()}))`

func executePython(ctx context.Context,pythonPath,script string,d *Decoded,r *HTTPResponse,c *Collector)(*MetricSet,error){
	if pythonPath==""{pythonPath="python3"};timeout:=time.Duration(c.Limits.ScriptTimeout);if timeout<=0{timeout=100*time.Millisecond};pctx,cancel:=context.WithTimeout(ctx,timeout);defer cancel()
	input:=pythonInput{Script:script,Data:d.Data,Target:r.Target,Collector:c.Name,Response:pythonResponse{StatusCode:r.StatusCode,Headers:r.Headers,Body:string(r.Body),Text:string(r.Body)}};b,err:=json.Marshal(input);if err!=nil{return nil,err};cmd:=exec.CommandContext(pctx,pythonPath,"-I","-c",pythonLauncher);cmd.Stdin=bytes.NewReader(b);var stdout,stderr bytes.Buffer;cmd.Stdout=&stdout;cmd.Stderr=&stderr;err=cmd.Run();if pctx.Err()!=nil{return nil,fmt.Errorf("Python execution timeout: %w",pctx.Err())};if err!=nil{return nil,fmt.Errorf("Python execution failed: %w: %s",err,stderr.String())};if c.Limits.MaxOutputBytes>0&&stdout.Len()>c.Limits.MaxOutputBytes{return nil,fmt.Errorf("Python output exceeds limit")};var result struct{Metrics []Metric `json:"metrics"`};if err=json.Unmarshal(stdout.Bytes(),&result);err!=nil{return nil,fmt.Errorf("Python output: %w",err)};return &MetricSet{Metrics:result.Metrics},nil
}
