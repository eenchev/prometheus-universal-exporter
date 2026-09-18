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

func fetch(ctx context.Context, target string, c *Collector, forwarded ...http.Header) (*HTTPResponse,error) {
	u,err:=url.Parse(target);if err!=nil{return nil,fmt.Errorf("invalid target: %w",err)};if u.Scheme==""{u,err=url.Parse("http://"+target);if err!=nil{return nil,fmt.Errorf("invalid target: %w",err)}}
	allowed:=c.Request.AllowedSchemes;if len(allowed)==0{allowed=[]string{"http","https"}};ok:=false;for _,s:=range allowed{if strings.EqualFold(s,u.Scheme){ok=true}};if !ok{return nil,fmt.Errorf("target scheme %q is not allowed",u.Scheme)};if u.Host==""{return nil,fmt.Errorf("target has no host")}
	if c.Request.Path!="" {base:=strings.TrimSuffix(u.Path,"/");p:=strings.TrimPrefix(c.Request.Path,"/");u.Path=path.Join("/",base,p);if strings.HasSuffix(c.Request.Path,"/"){u.Path+="/"}}
	q:=u.Query();for k,v:=range c.Request.Query{q.Set(k,v)};u.RawQuery=q.Encode()
	tlsCfg,err:=tlsConfig(c.Request.TLS);if err!=nil{return nil,err};policy:=c.Request.RedirectPolicy;client:=&http.Client{Timeout:time.Duration(c.Request.Timeout),Transport:&http.Transport{TLSClientConfig:tlsCfg}};if strings.EqualFold(policy,"none")||strings.EqualFold(policy,"reject"){client.CheckRedirect=func(_ *http.Request,_ []*http.Request)error{return http.ErrUseLastResponse}}
	method:=c.Request.Method;reqBody:=io.Reader(nil);if c.Request.Body!=""{reqBody=strings.NewReader(c.Request.Body)};req,err:=http.NewRequestWithContext(ctx,method,u.String(),reqBody);if err!=nil{return nil,err};for k,v:=range c.Request.Headers{req.Header.Set(k,v)};if c.Request.BasicAuth!=nil{req.SetBasicAuth(c.Request.BasicAuth.Username,c.Request.BasicAuth.Password)};if c.Request.BasicAuthFile!=nil{username,readErr:=readCredentialFile(c.Request.BasicAuthFile.Username);if readErr!=nil{return nil,fmt.Errorf("reading basic auth username file: %w",readErr)};password,readErr:=readCredentialFile(c.Request.BasicAuthFile.Password);if readErr!=nil{return nil,fmt.Errorf("reading basic auth password file: %w",readErr)};if username==""||password==""{return nil,fmt.Errorf("basic auth credential files must not be empty")};req.SetBasicAuth(username,password)};bearerToken:=c.Request.BearerToken;if c.Request.BearerTokenFile!=""{token,readErr:=os.ReadFile(c.Request.BearerTokenFile);if readErr!=nil{return nil,fmt.Errorf("reading bearer token file: %w",readErr)};bearerToken=strings.TrimSpace(string(token));if bearerToken==""{return nil,fmt.Errorf("bearer token file %s is empty",c.Request.BearerTokenFile)}};if bearerToken!=""{req.Header.Set("Authorization","Bearer "+bearerToken)};if len(forwarded)>0{for k,v:=range forwarded[0]{req.Header[k]=append([]string(nil),v...)}}
	start:=time.Now();resp,err:=client.Do(req);if err!=nil{return nil,fmt.Errorf("HTTP request failed: %w",err)};defer resp.Body.Close();limit:=c.Limits.MaxResponseBytes;if limit<=0||c.Request.MaxResponseBytes>0&&c.Request.MaxResponseBytes<limit{limit=c.Request.MaxResponseBytes};if limit<=0{limit=10<<20};body,err:=io.ReadAll(io.LimitReader(resp.Body,limit+1));if err!=nil{return nil,fmt.Errorf("reading response: %w",err)};if int64(len(body))>limit{return nil,fmt.Errorf("response size %d exceeds limit %d",len(body),limit)};return &HTTPResponse{StatusCode:resp.StatusCode,Headers:resp.Header.Clone(),Body:body,Target:target,Collector:c.Name,Duration:time.Since(start)},nil
}

func readCredentialFile(path string) (string, error) { if strings.TrimSpace(path) == "" { return "", fmt.Errorf("path is empty") }; b, err := os.ReadFile(path); if err != nil { return "", err }; return strings.TrimSpace(string(b)), nil }
