package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type HTTPResponse struct { StatusCode int; Headers http.Header; Body []byte; Target string; Collector string; Duration time.Duration }

type RequestOverrides struct { Method string; Path string; PathSet bool; Timeout time.Duration; Body *string }

func parseRequestOverrides(values url.Values) (RequestOverrides, error) {
	overrides := RequestOverrides{}
	if method := strings.TrimSpace(values.Get("method")); method != "" {
		overrides.Method = strings.ToUpper(method)
		switch overrides.Method { case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD": default: return overrides, fmt.Errorf("unsupported request method override %q", method) }
	}
	if path, ok := values["path"]; ok { overrides.PathSet = true; if len(path) > 0 { overrides.Path = path[0] } }
	if timeout := strings.TrimSpace(values.Get("timeout")); timeout != "" { parsed, err := time.ParseDuration(timeout); if err != nil || parsed <= 0 { return overrides, fmt.Errorf("invalid request timeout override %q", timeout) }; overrides.Timeout = parsed }
	if body, ok := values["body"]; ok { value := ""; if len(body) > 0 { value = body[0] }; overrides.Body = &value }
	return overrides, nil
}

func fetch(ctx context.Context, target string, c *Collector, overrides RequestOverrides, forwarded ...http.Header) (*HTTPResponse,error) {
	u,err:=url.Parse(target);if err!=nil{return nil,fmt.Errorf("invalid target: %w",err)};if u.Scheme==""{u,err=url.Parse("http://"+target);if err!=nil{return nil,fmt.Errorf("invalid target: %w",err)}}
	allowed:=c.Request.AllowedSchemes;if len(allowed)==0{allowed=[]string{"http","https"}};ok:=false;for _,s:=range allowed{if strings.EqualFold(s,u.Scheme){ok=true}};if !ok{return nil,fmt.Errorf("target scheme %q is not allowed",u.Scheme)};if u.Host==""{return nil,fmt.Errorf("target has no host")}
	requestPath := c.Request.Path; if overrides.PathSet { requestPath = overrides.Path }; if requestPath!="" {base:=strings.TrimSuffix(u.Path,"/");p:=strings.TrimPrefix(requestPath,"/");u.Path=path.Join("/",base,p);if strings.HasSuffix(requestPath,"/"){u.Path+="/"}}
	q:=u.Query();for k,v:=range c.Request.Query{q.Set(k,v)};u.RawQuery=q.Encode()
	tlsCfg,err:=tlsConfig(c.Request.TLS);if err!=nil{return nil,err};policy:=c.Request.RedirectPolicy;client:=&http.Client{Transport:&http.Transport{TLSClientConfig:tlsCfg}};if strings.EqualFold(policy,"none")||strings.EqualFold(policy,"reject"){client.CheckRedirect=func(_ *http.Request,_ []*http.Request)error{return http.ErrUseLastResponse}}
	requestContext:=ctx;cancel:=func(){};if overrides.Timeout>0{requestContext,cancel=context.WithTimeout(ctx,overrides.Timeout)};defer cancel();method:=c.Request.Method;if overrides.Method!=""{method=overrides.Method};requestBody:=c.Request.Body;if overrides.Body!=nil{requestBody=*overrides.Body};reqBody:=io.Reader(nil);if requestBody!=""{reqBody=strings.NewReader(requestBody)};req,err:=http.NewRequestWithContext(requestContext,method,u.String(),reqBody);if err!=nil{return nil,err};for k,v:=range c.Request.Headers{req.Header.Set(k,v)};if c.Request.BasicAuth!=nil{req.SetBasicAuth(c.Request.BasicAuth.Username,c.Request.BasicAuth.Password)};if c.Request.BasicAuthFile!=nil{username,readErr:=readCredentialFile(c.Request.BasicAuthFile.Username);if readErr!=nil{return nil,fmt.Errorf("reading basic auth username file: %w",readErr)};password,readErr:=readCredentialFile(c.Request.BasicAuthFile.Password);if readErr!=nil{return nil,fmt.Errorf("reading basic auth password file: %w",readErr)};if username==""||password==""{return nil,fmt.Errorf("basic auth credential files must not be empty")};req.SetBasicAuth(username,password)};bearerToken:=c.Request.BearerToken;if c.Request.BearerTokenFile!=""{token,readErr:=os.ReadFile(c.Request.BearerTokenFile);if readErr!=nil{return nil,fmt.Errorf("reading bearer token file: %w",readErr)};bearerToken=strings.TrimSpace(string(token));if bearerToken==""{return nil,fmt.Errorf("bearer token file %s is empty",c.Request.BearerTokenFile)}};if bearerToken!=""{req.Header.Set("Authorization","Bearer "+bearerToken)};if len(forwarded)>0{for k,v:=range forwarded[0]{req.Header[k]=append([]string(nil),v...)}}
	start:=time.Now();resp,err:=client.Do(req);if err!=nil{return nil,fmt.Errorf("HTTP request failed: %w",err)};defer resp.Body.Close();limit:=c.Limits.MaxResponseBytes;if limit<=0||c.Request.MaxResponseBytes>0&&c.Request.MaxResponseBytes<limit{limit=c.Request.MaxResponseBytes};if limit<=0{limit=10<<20};body,err:=io.ReadAll(io.LimitReader(resp.Body,limit+1));if err!=nil{return nil,fmt.Errorf("reading response: %w",err)};if int64(len(body))>limit{return nil,fmt.Errorf("response size %d exceeds limit %d",len(body),limit)};return &HTTPResponse{StatusCode:resp.StatusCode,Headers:resp.Header.Clone(),Body:body,Target:target,Collector:c.Name,Duration:time.Since(start)},nil
}

func readCredentialFile(path string) (string, error) { if strings.TrimSpace(path) == "" { return "", fmt.Errorf("path is empty") }; b, err := os.ReadFile(path); if err != nil { return "", err }; return strings.TrimSpace(string(b)), nil }
