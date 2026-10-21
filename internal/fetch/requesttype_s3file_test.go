//go:build request_type_s3file

package fetch

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// fakeS3 answers the three calls of the S3 API an s3file read makes —
// HeadObject, GetObject and ListObjectsV2 — with path-style addressing, from
// objects in memory. It does not check signatures; it keeps each request's
// Authorization header, so a test can see which credentials and region
// signed it.
type fakeS3 struct {
	server *httptest.Server
	mu     sync.Mutex
	// buckets holds each bucket's objects by key.
	buckets map[string]map[string]*fakeObject
	// requests are the requests made, as "HEAD key", "GET key" and "LIST
	// prefix"; auth their Authorization headers.
	requests []string
	auth     []string
	// page is the most entries one listing answers, whatever it asks for.
	page int
	// denied is a bucket every request to which is answered 403.
	denied string
	// afterList, when set, runs after each listing is answered.
	afterList func()
	// hold, when set, is waited on before a GetObject is answered.
	hold chan struct{}
}

type fakeObject struct {
	body     string
	modified time.Time
	etag     string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{buckets: map[string]map[string]*fakeObject{}, page: 1000}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	// The SDK's default chain is the environment's and the shared files':
	// a test has its own, and asks no metadata service.
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "none"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "none"),
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "",
		"AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_PROFILE": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(func() { s3Clients.follow(nil, nil) })
	return f
}

// put stores an object, last modified at modified.
func (f *fakeS3) put(bucket, key, body string, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[bucket] == nil {
		f.buckets[bucket] = map[string]*fakeObject{}
	}
	f.buckets[bucket][key] = &fakeObject{body: body, modified: modified.UTC().Truncate(time.Second), etag: fmt.Sprintf(`"%x"`, len(body)*7919+modified.Second())}
}

// made returns the requests made so far.
func (f *fakeS3) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	objects, exists := f.buckets[bucket]
	denied := bucket == f.denied
	f.mu.Unlock()
	if denied {
		s3ErrorAnswer(w, r, http.StatusForbidden, "AccessDenied")
		return
	}
	if !exists {
		s3ErrorAnswer(w, r, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if key == "" && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r, bucket, objects)
		return
	}
	if key == "" && r.Method == http.MethodHead {
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+key)
	object := objects[key]
	var o fakeObject
	if object != nil {
		o = *object
	}
	hold := f.hold
	f.mu.Unlock()
	if object == nil {
		s3ErrorAnswer(w, r, http.StatusNotFound, "NoSuchKey")
		return
	}
	if hold != nil && r.Method == http.MethodGet {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
	w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
	w.Header().Set("ETag", o.etag)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(o.body))
	}
}

func s3ErrorAnswer(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
	}
}

type fakeListResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
	Contents              []struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int    `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// list answers a ListObjectsV2 with the delimiter /, as S3 does: the
// objects directly under the prefix and the common prefixes below it, in
// key order, a page at a time, the continuation token being where the next
// page starts.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request, bucket string, objects map[string]*fakeObject) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	maxKeys, _ := strconv.Atoi(q.Get("max-keys"))
	start, _ := strconv.Atoi(q.Get("continuation-token"))
	f.mu.Lock()
	f.requests = append(f.requests, "LIST "+prefix)
	page := min(maxKeys, f.page)
	type item struct {
		key    string
		object *fakeObject
	}
	seen := map[string]bool{}
	var items []item
	for key, object := range objects {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		if dir, _, nested := strings.Cut(rest, "/"); nested && q.Get("delimiter") == "/" {
			if !seen[dir] {
				seen[dir] = true
				items = append(items, item{key: prefix + dir + "/"})
			}
			continue
		}
		o := *object
		items = append(items, item{key: key, object: &o})
	}
	after := f.afterList
	f.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	result := fakeListResult{Name: bucket, Prefix: prefix, MaxKeys: maxKeys}
	end := min(start+page, len(items))
	for _, it := range items[min(start, len(items)):end] {
		if it.object == nil {
			result.CommonPrefixes = append(result.CommonPrefixes, struct {
				Prefix string `xml:"Prefix"`
			}{it.key})
			continue
		}
		result.Contents = append(result.Contents, struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
			ETag         string `xml:"ETag"`
			Size         int    `xml:"Size"`
		}{it.key, it.object.modified.Format("2006-01-02T15:04:05.000Z"), it.object.etag, len(it.object.body)})
	}
	result.KeyCount = len(result.Contents) + len(result.CommonPrefixes)
	if end < len(items) {
		result.IsTruncated, result.NextContinuationToken = true, strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
	if after != nil {
		after()
	}
}

// s3Credentials writes static credential files and returns the request
// keys that name them.
func s3Credentials(t *testing.T, id string) func(*model.RequestConfig) {
	t.Helper()
	dir := t.TempDir()
	idFile := testutil.WriteIn(t, dir, "id", id+"\n")
	secretFile := testutil.WriteIn(t, dir, "secret", "s3cret\n")
	return func(r *model.RequestConfig) {
		r.AccessKeyIDFile, r.SecretAccessKeyFile = idFile, secretFile
	}
}

// collector is a validated s3file collector of the fake store reading
// path under /exports/app with static credentials, changed by change
// first.
func (f *fakeS3) collector(t *testing.T, name, path string, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: RequestTypeS3File, Root: "/exports/app", Path: path, Endpoint: f.server.URL, PathStyle: true, Region: "eu-west-1"},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
	s3Credentials(t, "AKIDTEST")(&c.Request)
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// An object is read as a file is: its body is the response's, with no
// status, and its size, modification time and entity tag are the
// Content-Length, Last-Modified and ETag headers transforms see. The request
// is path-style, to the configured endpoint, and signed with the access key
// of the credential files for the configured region.
func TestS3FileReadsAnObjectAsAFile(t *testing.T) {
	f := newFakeS3(t)
	modified := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	f.put("metrics", "exports/app/status.prom", "up 1\n", modified)
	c := f.collector(t, "objects", "status.prom", s3Credentials(t, "AKIDFROMFILE"))
	resp, err := FetchCollector(context.Background(), "s3://metrics", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "up 1\n" || !resp.NoStatus || resp.Target != "s3://metrics" {
		t.Fatalf("body %q, no status %v, target %q", resp.Body, resp.NoStatus, resp.Target)
	}
	f.mu.Lock()
	etag := f.buckets["metrics"]["exports/app/status.prom"].etag
	auth := slices.Clone(f.auth)
	f.mu.Unlock()
	for header, want := range map[string]string{"Content-Length": "5", "Last-Modified": modified.Format(http.TimeFormat), "ETag": etag, "Content-Type": "text/plain; version=0.0.4"} {
		if got := resp.Headers.Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	if got := f.made(); !slices.Equal(got, []string{"HEAD exports/app/status.prom", "GET exports/app/status.prom"}) {
		t.Errorf("requests %v, want a HeadObject and a GetObject of the key", got)
	}
	for _, a := range auth {
		if !strings.Contains(a, "Credential=AKIDFROMFILE/") || !strings.Contains(a, "/eu-west-1/s3/aws4_request") {
			t.Errorf("Authorization %q, want SigV4 with the file's access key for eu-west-1", a)
		}
	}
	if label, err := RequestLabelFor("metrics", c, RequestOverrides{}); err != nil || label != "s3://metrics/exports/app/status.prom" {
		t.Errorf("url label %q, %v", label, err)
	}
	if RequestMethodFor(c, RequestOverrides{}) != "READ" || FetchStage(c) != "s3" {
		t.Errorf("method %q, stage %q", RequestMethodFor(c, RequestOverrides{}), FetchStage(c))
	}
}

// The credential files are read at every request, so a rotated key is used
// by the next scrape, with the client and its connections kept.
func TestS3FileReadsItsCredentialFilesAtEveryRequest(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now())
	c := f.collector(t, "rotated", "a.prom", s3Credentials(t, "AKIDFIRST"))
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Request.AccessKeyIDFile, []byte("AKIDSECOND\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	auth := slices.Clone(f.auth)
	f.mu.Unlock()
	if len(auth) != 4 || !strings.Contains(auth[1], "Credential=AKIDFIRST/") || !strings.Contains(auth[3], "Credential=AKIDSECOND/") {
		t.Fatalf("Authorization headers %q, want the first key and then the rotated one", auth)
	}
	if err := os.WriteFile(c.Request.AccessKeyIDFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.access_key_id_file "+c.Request.AccessKeyIDFile+" is empty") {
		t.Fatalf("err=%v, want the empty key file named", err)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "is empty") {
		t.Errorf("--dry-run warnings %q, want the empty key file", warnings)
	}
}

// Without credential files, the SDK's default chain finds the credentials:
// here the environment's, with the region the environment names when the
// collector sets none.
func TestS3FileTakesTheSDKsDefaultCredentialsAndRegion(t *testing.T) {
	f := newFakeS3(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDFROMENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")
	t.Setenv("AWS_REGION", "ap-south-2")
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now())
	c := f.collector(t, "chain", "a.prom", func(r *model.RequestConfig) {
		r.Region, r.AccessKeyIDFile, r.SecretAccessKeyFile = "", "", ""
	})
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	auth := f.auth[0]
	f.mu.Unlock()
	if !strings.Contains(auth, "Credential=AKIDFROMENV/") || !strings.Contains(auth, "/ap-south-2/s3/") {
		t.Fatalf("Authorization %q, want the environment's key and region", auth)
	}
	if FileWarnings(c, nil) != nil {
		t.Error("--dry-run warns of a collector without credential files")
	}
}

// With request.files, the objects directly under the prefix whose names
// match are read as a localfile collector reads a directory's files, each on
// its own and with its modification time. A key below a further / is in a
// subdirectory, and the key of the prefix itself a console's folder
// marker: neither is a file. The listing is paged, and describes every
// object it lists, so no object is asked a HeadObject.
func TestS3FileReadsTheMatchingObjectsUnderAPrefix(t *testing.T) {
	f := newFakeS3(t)
	f.page = 2
	modified := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	for _, key := range []string{"exports/app/", "exports/app/a.prom", "exports/app/b.prom", "exports/app/notes.txt", "exports/app/old/c.prom", "exports/app/z.prom", "exports/other.prom"} {
		f.put("metrics", key, strings.TrimSuffix(filepath.Base(key), ".prom")+" 1\n", modified)
	}
	c := f.collector(t, "textfiles", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
	resp, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := resp.Directory
	if read == nil {
		t.Fatal("no directory read")
	}
	var names []string
	for _, file := range read.Files {
		if file.Err != nil {
			t.Errorf("%s: %v", file.Name, file.Err)
			continue
		}
		names = append(names, file.Name)
		if want := strings.TrimSuffix(file.Name, ".prom") + " 1\n"; string(file.Response.Body) != want || !file.ModTime.Equal(modified) || file.Response.Target != "metrics" {
			t.Errorf("%s: body %q, modified %v, target %q", file.Name, file.Response.Body, file.ModTime, file.Response.Target)
		}
	}
	if !slices.Equal(names, []string{"a.prom", "b.prom", "z.prom"}) || read.Matched != 3 {
		t.Errorf("read %v (matched %d), want a.prom, b.prom and z.prom", names, read.Matched)
	}
	var lists, heads int
	for _, request := range f.made() {
		switch {
		case request == "LIST exports/app/":
			lists++
		case strings.HasPrefix(request, "HEAD "):
			heads++
		}
	}
	if lists != 3 || heads != 0 {
		t.Errorf("requests %v: %d listings of exports/app/, %d HeadObjects; want the six entries in pages of two and no HeadObject", f.made(), lists, heads)
	}
	if label, err := RequestLabelFor("s3://metrics", c, RequestOverrides{}); err != nil || label != "s3://metrics/exports/app/" {
		t.Errorf("url label %q, %v", label, err)
	}
	if err := CheckOverrideParams(c, map[string][]string{"path": {"a.prom"}}); err == nil {
		t.Error("a collector reading a prefix took a path parameter")
	}
}

// An object replaced between the listing and its read is read again,
// described by its own answers rather than the listing's: the read
// succeeds with the new object.
func TestS3FileReadsAgainAnObjectReplacedSinceTheListing(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now().Add(-time.Hour))
	f.afterList = func() { f.put("metrics", "exports/app/a.prom", "a 22\n", time.Now()) }
	c := f.collector(t, "replaced", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
	resp, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if files := resp.Directory.Files; len(files) != 1 || files[0].Err != nil || string(files[0].Response.Body) != "a 22\n" {
		t.Fatalf("files %+v, want the replaced object read", files)
	}
	if got := f.made(); !slices.Equal(got, []string{"LIST exports/app/", "GET exports/app/a.prom", "HEAD exports/app/a.prom", "GET exports/app/a.prom"}) {
		t.Errorf("requests %v, want the object read again after a HeadObject", got)
	}
}

// A missing object, a missing bucket and a bucket the credentials may not
// read each fail the scrape saying so, naming the key and the bucket.
func TestS3FileFailuresNameTheObjectAndTheBucket(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now())
	f.put("locked", "exports/app/a.prom", "a 1\n", time.Now())
	f.denied = "locked"
	for _, tc := range []struct{ target, path, want string }{
		{"metrics", "gone.prom", "file /exports/app/gone.prom does not exist (in s3://metrics)"},
		{"nowhere", "a.prom", "the bucket does not exist"},
		{"locked", "a.prom", "access denied (403): the credentials need s3:GetObject on the object"},
	} {
		c := f.collector(t, "failing", tc.path)
		_, err := FetchCollector(context.Background(), tc.target, c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "(in s3://"+tc.target+")") {
			t.Errorf("%s/%s: err=%v, want %q and the bucket", tc.target, tc.path, err, tc.want)
		}
	}
}

// The response limit and max_age hold for an object as for a file.
func TestS3FileHoldsAnObjectToTheResponseLimitAndMaxAge(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/big.prom", strings.Repeat("x", 2048), time.Now())
	f.put("metrics", "exports/app/stale.prom", "a 1\n", time.Now().Add(-3*time.Hour))
	c := f.collector(t, "limited", "big.prom", func(r *model.RequestConfig) {
		r.MaxResponseBytes = 1024
		r.MaxAge = model.Duration(time.Hour)
	})
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("an object over the limit: err=%v, want ErrLimitExceeded", err)
	}
	_, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{Path: "stale.prom", PathSet: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "longer than request.max_age 1h0m0s") {
		t.Errorf("a stale object: err=%v, want it refused for its age", err)
	}
}

// A path parameter names the object as it names a file, held to localfile's
// rules.
func TestS3FilePathParametersNameTheObject(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/nightly.json", `{"ok":1}`, time.Now())
	c := f.collector(t, "jobs", "{{param_job}}.json")
	resp, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{Params: map[string]string{"param_job": "nightly"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"ok":1}` {
		t.Fatalf("body %q", resp.Body)
	}
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{Params: map[string]string{"param_job": "../x"}}, nil); err == nil {
		t.Error("a path parameter with / in it was taken")
	}
	if label, _ := RequestLabelFor("metrics", c, RequestOverrides{}); label != "s3://metrics/exports/app/{{param_job}}.json" {
		t.Errorf("url label %q, want the placeholder kept", label)
	}
}

// The probe's deadline holds for a store that does not answer.
func TestS3FileReturnsAtTheProbesDeadline(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now())
	f.hold = make(chan struct{})
	defer close(f.hold)
	c := f.collector(t, "slow", "a.prom")
	start := time.Now()
	_, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{Timeout: 200 * time.Millisecond}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s, want the deadline", err, time.Since(start))
	}
}

// A target is a bucket, written as it is or as s3://bucket, and nothing
// else: a key, a query, another scheme or a name no bucket has is refused.
func TestS3FileTargetIsABucket(t *testing.T) {
	c := &model.Collector{Name: "t", Request: model.RequestConfig{Type: RequestTypeS3File, Root: "/"}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, bucket string }{{"metrics", "metrics"}, {"s3://metrics", "metrics"}, {" s3://my.bucket-1/ ", "my.bucket-1"}} {
		if got, err := parseS3Target(tc.target); err != nil || got != tc.bucket {
			t.Errorf("%q: bucket %q, %v; want %q", tc.target, got, err, tc.bucket)
		}
		if err := CheckTarget(c, tc.target, false); err != nil {
			t.Errorf("%q refused: %v", tc.target, err)
		}
	}
	for _, target := range []string{"s3://metrics/key.prom", "metrics?x=1", "https://metrics", "Metrics", "ab", "a..b", "-ab", "user@metrics"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("%q taken as a bucket", target)
		}
		if DisplayTarget(c, target) != "<invalid target>" {
			t.Errorf("%q shown as %q", target, DisplayTarget(c, target))
		}
	}
	if err := CheckTarget(c, "", false); !errors.Is(err, ErrMissingTarget) {
		t.Errorf("no target: %v, want it required", err)
	}
}

// allowed_targets and denied_targets name buckets, and a bucket they refuse
// is refused before anything is sent; an address in them names no bucket
// and is refused at load.
func TestS3FileTargetListsNameBuckets(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics-prod", "exports/app/a.prom", "a 1\n", time.Now())
	c := f.collector(t, "listed", "a.prom", func(r *model.RequestConfig) {
		r.AllowedTargets = []string{"metrics-*"}
		r.DeniedTargets = []string{"metrics-secret"}
	})
	if _, err := FetchCollector(context.Background(), "metrics-prod", c, RequestOverrides{}, nil); err != nil {
		t.Errorf("an allowed bucket: %v", err)
	}
	for _, bucket := range []string{"metrics-secret", "other"} {
		before := len(f.made())
		if _, err := FetchCollector(context.Background(), bucket, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: err=%v, want it refused", bucket, err)
		}
		if len(f.made()) != before {
			t.Errorf("%s: a request was sent for a refused bucket", bucket)
		}
	}
	bad := model.Collector{Name: "nets", Request: model.RequestConfig{Type: RequestTypeS3File, Root: "/", AllowedTargets: []string{"10.0.0.0/8"}}}
	if err := ValidateRequest(&bad); err == nil || !strings.Contains(err.Error(), "name buckets") {
		t.Errorf("an address in allowed_targets: %v", err)
	}
}

// The keys of an s3file collector are held to their rules at load.
func TestS3FileValidation(t *testing.T) {
	valid := model.RequestConfig{Type: RequestTypeS3File, Root: "/exports", Path: "a.prom"}
	for _, tc := range []struct {
		name   string
		change func(*model.RequestConfig)
		want   string
	}{
		{"no root", func(r *model.RequestConfig) { r.Root = "" }, "request.root is required"},
		{"relative root", func(r *model.RequestConfig) { r.Root = "exports" }, "must be an absolute path"},
		{"path above root", func(r *model.RequestConfig) { r.Path = "../a.prom" }, "leads outside request.root"},
		{"region", func(r *model.RequestConfig) { r.Region = "eu west" }, "is not a region"},
		{"endpoint scheme", func(r *model.RequestConfig) { r.Endpoint = "minio:9000" }, "is not an http:// or https:// URL"},
		{"endpoint path", func(r *model.RequestConfig) { r.Endpoint = "https://minio.example/bucket" }, "has a user, a path, a query or a fragment"},
		{"half a key", func(r *model.RequestConfig) { r.AccessKeyIDFile = "/id" }, "without the other"},
		{"token alone", func(r *model.RequestConfig) { r.SessionTokenFile = "/token" }, "the token is the temporary access key's"},
		{"blank file", func(r *model.RequestConfig) { r.AccessKeyIDFile, r.SecretAccessKeyFile = " ", "/s" }, "written with blanks alone"},
		{"placeholder", func(r *model.RequestConfig) { r.AccessKeyIDFile, r.SecretAccessKeyFile = "/{{param_x}}", "/s" }, "has a placeholder"},
		{"another type's key", func(r *model.RequestConfig) { r.Username = "metrics" }, "request.username, which does not apply"},
		{"path and files", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} }, "sets both request.path and request.files"},
	} {
		c := model.Collector{Name: "v", Request: valid}
		tc.change(&c.Request)
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
	for _, change := range []func(*model.RequestConfig){
		func(r *model.RequestConfig) { r.Root = "/" },
		func(r *model.RequestConfig) {
			r.Endpoint, r.PathStyle, r.Region = "http://127.0.0.1:9000/", true, "auto"
		},
		func(r *model.RequestConfig) {
			r.AccessKeyIDFile, r.SecretAccessKeyFile, r.SessionTokenFile = "/id", "/s", "/t"
		},
	} {
		c := model.Collector{Name: "v", Request: valid}
		change(&c.Request)
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("%+v refused: %v", c.Request, err)
		}
	}
}

// A static target may set path and timeout, and its path is held to the
// rules of a file's.
func TestS3FileStaticTargetRequest(t *testing.T) {
	c := &model.Collector{Name: "st", Request: model.RequestConfig{Type: RequestTypeS3File, Root: "/exports"}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	ok := &model.StaticTarget{Name: "a", Request: model.TargetRequestConfig{Path: "a.prom", PathSet: true}}
	if err := CheckTargetRequest(ok, c); err != nil {
		t.Errorf("a path: %v", err)
	}
	above := &model.StaticTarget{Name: "b", Request: model.TargetRequestConfig{Path: "../a.prom", PathSet: true}}
	if err := CheckTargetRequest(above, c); err == nil || !strings.Contains(err.Error(), "leads outside request.root") {
		t.Errorf("a path above root: %v", err)
	}
}

// A reload that leaves no collector with a client's settings drops the
// client; one that keeps them keeps it.
func TestS3FileClientsFollowTheConfiguration(t *testing.T) {
	f := newFakeS3(t)
	f.put("metrics", "exports/app/a.prom", "a 1\n", time.Now())
	c := f.collector(t, "followed", "a.prom")
	if _, err := FetchCollector(context.Background(), "metrics", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		s3Clients.mu.Lock()
		defer s3Clients.mu.Unlock()
		return len(s3Clients.clients)
	}
	FollowConfiguration([]model.Collector{*c}, nil)
	if count() != 1 {
		t.Fatalf("%d clients kept for the collector in force, want 1", count())
	}
	other := *c
	other.Request.Region = "us-west-2"
	FollowConfiguration([]model.Collector{other}, nil)
	if count() != 0 {
		t.Fatalf("%d clients kept after the reload, want none", count())
	}
}

// optInRequestKeys lists the keys s3file accepts that no type of a default
// build does: each it lists is the type's and no other built type's, and,
// in a build with every other type, each key of the type's that no other
// type accepts is listed.
func TestTheOptInRequestKeysAreTheTypesOwn(t *testing.T) {
	own := RequestTypes[RequestTypeS3File]
	listed := optInRequestKeys[RequestTypeS3File]
	everyOther := true
	for _, name := range knownRequestTypes {
		if name != RequestTypeS3File && RequestTypes[name] == nil {
			everyOther = false
		}
	}
	for _, key := range own.Fields {
		var others []string
		for name, rt := range RequestTypes {
			if name != RequestTypeS3File && slices.Contains(rt.Fields, key) {
				others = append(others, name)
			}
		}
		switch {
		case slices.Contains(listed, key) && len(others) > 0:
			t.Errorf("optInRequestKeys lists %s, which %v accept too", key, others)
		case !slices.Contains(listed, key) && len(others) == 0 && everyOther:
			t.Errorf("s3file accepts %s, which no other type does, and optInRequestKeys does not list it", key)
		}
	}
	for _, key := range listed {
		if !slices.Contains(own.Fields, key) {
			t.Errorf("optInRequestKeys lists %s, which s3file does not accept", key)
		}
	}
}
