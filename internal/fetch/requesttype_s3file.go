//go:build request_type_s3file

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The s3file request type is localfile for an object store: it reads an
// object, or every matching object under a prefix, from a bucket of Amazon
// S3 or of a store that speaks its API — MinIO, Ceph's RADOS Gateway,
// Cloudflare R2, Google Cloud Storage's XML API — and decodes, transforms
// and checks it exactly as localfile does a file.
//
//	request:
//	  type: s3file
//	  root: /exports/app               # required: the key prefix, as a directory
//	  path: status.json                # or files: ["*.prom"]
//	  region: eu-west-1                # optional: the SDK's, else us-east-1
//	  endpoint: https://minio.example  # optional: a store other than AWS
//	  path_style: true                 # optional: the bucket in the URL's path
//
// The probe's target is the bucket, bucket or s3://bucket. Everything the
// read shares with localfile — the path rules, a file read again when it
// changes under the read, max_age, the response limit, the directory rules
// — is the same code (filereads.go, localfile_directory.go), over the S3 API
// instead of an os.Root (s3source.go).
//
//   - Credentials are the AWS SDK's default chain — the environment, a web
//     identity token (IRSA), the shared files, the container's or the
//     instance's role — unless access_key_id_file and secret_access_key_file
//     name static ones, which are read at every request, as every other
//     credential file is.
//   - A request is signed with SigV4 for the region, and sent once: the
//     SDK's retries are switched off, as file reads retry nothing, and the
//     next scrape tries again.
//   - allowed_targets and denied_targets name buckets, by name or glob,
//     since a target is a bucket: the store reached is the configuration's.
//
// The type is opt-in: a default build leaves it out, with the AWS SDK it
// alone of the types imports. This file and s3source.go are built under its
// tag alone, so only a build that asks for it links the SDK's S3 client.

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeS3File,
		Fields: []string{
			"root", "path", "files", "max_files", "max_total_bytes", "max_age", "max_response_bytes",
			"region", "endpoint", "path_style", "access_key_id_file", "secret_access_key_file", "session_token_file",
			"allowed_targets", "denied_targets",
		},
		Overrides:      []string{"path", "timeout", PathParamPrefix},
		TargetFields:   []string{"path", "timeout"},
		Requester:      s3FileRequester{},
		FileExtensions: true,
		TargetHint:     func(*model.Collector) string { return "the bucket, or s3://bucket" },
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, fmt.Sprintf("its request.path (%q) or its label values", c.Request.Path))
		},
		SchemaRule: s3FileSchemaRule,
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error {
			if t.Request.PathSet {
				return checkLocalFileName("request.path", t.Request.Path)
			}
			return nil
		},
		Stage: "s3",
		CheckOverride: func(c *model.Collector, key string) error {
			// As for localfile: a param_ parameter can still fill a label
			// value of a collector reading a directory.
			if readsDirectory(c) && (key == "path" || strings.HasPrefix(key, PathParamPrefix) && c.LabelParams == nil) {
				return fmt.Errorf("collector %q reads every object under request.root that request.files matches, so there is no file for it to name", c.Name)
			}
			return nil
		},
		CheckFiles: checkS3Files,
		Follow:     s3Clients.follow,
	})
}

// s3FileRequester is the s3file request type's Requester.
type s3FileRequester struct{}

// Validate checks an s3file collector's request and fills in its defaults.
func (s3FileRequester) Validate(c *model.Collector) error {
	return validateS3FileRequest(c)
}

// Fetch performs one request of an s3file collector.
func (s3FileRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchS3File(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks an s3file collector's target.
func (s3FileRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseS3Target(target)
	return err
}

// Label is the url label of an s3file request.
func (s3FileRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return s3FileLabel(target, c, overrides)
}

// Method is the http_method label of an s3file request.
func (s3FileRequester) Method(*model.Collector, RequestOverrides) string { return localFileMethod }

// Display renders an s3file target for logs and labels.
func (s3FileRequester) Display(target string) string {
	return s3Display(target)
}

// s3DefaultRegion is the region of a collector that sets none, when the
// SDK's chain finds none either: AWS's first, and what MinIO and most other
// stores take when they are given any.
const s3DefaultRegion = "us-east-1"

// s3RegionPattern is what a region is written with: AWS's are lower-case
// words joined by hyphens, and other stores name theirs as they like —
// Ceph's "default", R2's "auto".
var s3RegionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// s3BucketPattern is a bucket name as S3 has them: 3 to 63 lower-case
// letters, digits, dots and hyphens, starting and ending with a letter or a
// digit. Every store that speaks the API takes no more than that.
var s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func validateS3FileRequest(x *model.Collector) error {
	if err := validateFileRequest(x, RequestTypeS3File, false); err != nil {
		return err
	}
	if err := validateLocalDirectory(x); err != nil {
		return err
	}
	r := &x.Request
	if r.Region != "" && !s3RegionPattern.MatchString(r.Region) {
		return fmt.Errorf("collector %q request.region %q is not a region; write it as the store names it, such as eu-west-1, with letters, digits, - and _ alone", x.Name, r.Region)
	}
	if r.Endpoint != "" {
		if err := checkS3Endpoint(r.Endpoint); err != nil {
			return fmt.Errorf("collector %q request.endpoint %w", x.Name, err)
		}
	}
	if err := checkS3CredentialKeys(r.AccessKeyIDFile, r.SecretAccessKeyFile, r.SessionTokenFile); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	policy, err := compileTargetPolicy(r.AllowedTargets, r.DeniedTargets)
	if err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if len(policy.allowNets)+len(policy.denyNets) > 0 {
		return fmt.Errorf("collector %q request.allowed_targets and request.denied_targets of an s3file collector name buckets, by name or by a glob such as metrics-*; an address or a network names none, since the store a request reaches is request.endpoint's", x.Name)
	}
	return nil
}

// checkS3Endpoint holds request.endpoint to what the SDK sends requests to:
// an http or https URL of a host, with a port or not, and nothing after
// it, since the bucket and the key are the SDK's to add.
func checkS3Endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q is not an http:// or https:// URL", raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%q names no host", raw)
	}
	if u.User != nil || strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("%q has a user, a path, a query or a fragment; it is the store's scheme, host and port alone, such as https://minio.example.com:9000, the bucket being the target and the key request.root and request.path", raw)
	}
	return nil
}

// checkS3CredentialKeys holds the static credential files to their rules:
// the access key's two halves together, a session token only with them,
// none written with blanks alone, and no placeholder in any, since a
// credential file is named as it is.
func checkS3CredentialKeys(accessKeyIDFile, secretAccessKeyFile, sessionTokenFile string) error {
	keys := []struct{ name, value string }{{"request.access_key_id_file", accessKeyIDFile}, {"request.secret_access_key_file", secretAccessKeyFile}, {"request.session_token_file", sessionTokenFile}}
	for _, key := range keys {
		if key.value != "" && strings.TrimSpace(key.value) == "" {
			return fmt.Errorf("%s is written with blanks alone; name the file, or leave the key out", key.name)
		}
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("%s %q has a placeholder; a credential file is named as it is, so it takes none", key.name, key.value)
		}
	}
	if (accessKeyIDFile == "") != (secretAccessKeyFile == "") {
		return errors.New("sets one of request.access_key_id_file and request.secret_access_key_file without the other; a static access key is the two together, and without either the AWS SDK's default chain finds the credentials")
	}
	if sessionTokenFile != "" && accessKeyIDFile == "" {
		return errors.New("sets request.session_token_file without request.access_key_id_file and request.secret_access_key_file; the token is the temporary access key's")
	}
	return nil
}

// parseS3Target reads a target: a bucket, written as it is or as
// s3://bucket. Nothing may come with it: the key is request.root and
// request.path.
func parseS3Target(raw string) (string, error) {
	bucket := strings.TrimSpace(raw)
	if rest, found := strings.CutPrefix(bucket, "s3://"); found {
		bucket = strings.TrimSuffix(rest, "/")
	} else if strings.Contains(bucket, "://") {
		return "", fmt.Errorf("target %q has a scheme other than s3://; an s3file target is the bucket, as bucket or s3://bucket", raw)
	}
	if strings.ContainsAny(bucket, "/?#@") {
		return "", fmt.Errorf("target %q has a key, a query or a user in it; an s3file target is the bucket alone, as bucket or s3://bucket: the key is request.root and request.path", raw)
	}
	if !s3BucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return "", fmt.Errorf("target %q is not a bucket name: 3 to 63 lower-case letters, digits, dots and hyphens, starting and ending with a letter or a digit", raw)
	}
	return bucket, nil
}

// s3Display renders a target for logs and labels: as written, since an
// s3file target holds no credentials, when it is one at all.
func s3Display(target string) string {
	if _, err := parseS3Target(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// s3URL is the s3:// URL of a key in a bucket.
func s3URL(bucket, key string) string {
	return "s3://" + bucket + "/" + key
}

// s3NoFile is the error of a read that names no file.
const s3NoFile = "no object to read: set request.path, or give the probe a path parameter naming one under request.root"

// s3FileLabel is the url label of a read: the object's s3:// URL, with path
// parameters as their placeholders, or the prefix's, ending in /, for a
// collector reading a directory.
func s3FileLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	bucket, err := parseS3Target(target)
	if err != nil {
		return "", err
	}
	if readsDirectory(c) {
		return s3URL(bucket, s3Prefix(c.Request.Root)), nil
	}
	file, err := fileUnderRoot("", c, overrides, false, s3NoFile)
	if err != nil {
		return "", err
	}
	return s3URL(bucket, s3Prefix(c.Request.Root)+file), nil
}

// checkBucket applies the collector's allowed_targets and denied_targets to
// a bucket, as names: the lists hold no address (validateS3FileRequest).
// The cloud metadata addresses the other types refuse by default are not
// checked, since the host a request reaches is the configuration's
// endpoint, or AWS's, and never one a probe names.
func checkBucket(policy *targetPolicy, bucket string) error {
	if policy == nil {
		return nil
	}
	allowed, err := policy.checkName(bucket)
	if err != nil {
		return err
	}
	if !allowed && len(policy.allowNames) > 0 {
		return &TargetRefusedError{Host: bucket, Reason: "it is not in request.allowed_targets"}
	}
	return nil
}

// fetchS3File reads a scrape's object, or directory, from the target bucket.
func fetchS3File(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	bucket, err := parseS3Target(target)
	if err != nil {
		return nil, err
	}
	file := ""
	if !readsDirectory(c) {
		if file, err = fileUnderRoot("", c, overrides, true, s3NoFile); err != nil {
			return nil, err
		}
	}
	if err := checkBucket(policyOf(c), bucket); err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	client, err := s3Clients.client(ctx, s3SettingsOf(c))
	if err != nil {
		return nil, err
	}
	src := &s3Source{ctx: ctx, client: client, bucket: bucket, prefix: s3Prefix(c.Request.Root)}
	if readsDirectory(c) {
		traceRequest(ctx, localFileMethod, s3URL(bucket, src.prefix), nil, "", false)
		progress := &directoryProgress{}
		done := make(chan error, 1)
		go func() { done <- readSourceDirectory(src, c, "", progress) }()
		read, err := awaitDirectory(ctx, c.Request.Root, progress, done, func() {})
		if err != nil {
			traceOutcome(ctx, "error: "+err.Error())
			return nil, onBucket(err, bucket)
		}
		traceOutcome(ctx, fmt.Sprintf("read %d files", len(read.Files)))
		for i := range read.Files {
			if f := read.Files[i].Response; f != nil {
				f.Target = target
			}
		}
		return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Target: target, Collector: c.Name, Duration: time.Since(start), Directory: read}, nil
	}
	traceRequest(ctx, localFileMethod, s3URL(bucket, src.key(file)), nil, "", false)
	body, info, err := readSourceFile(src, c.Request.Root, file, responseLimit(c))
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
		return nil, onBucket(err, bucket)
	}
	traceOutcome(ctx, "read")
	if err := checkMaxAge(c, path.Join(c.Request.Root, file), info.ModTime()); err != nil {
		return nil, onBucket(err, bucket)
	}
	resp := localFileResponse(file, body, info, c)
	// The entity tag tells one version of an object from another, which a
	// transform may want where the modification time is too coarse.
	if object, ok := info.(s3Info); ok && object.etag != "" {
		resp.Headers.Set("ETag", object.etag)
	}
	resp.Target, resp.Duration = target, time.Since(start)
	return resp, nil
}

// onBucket names the bucket in a failed read's error, which names the key
// alone, as a file's path.
func onBucket(err error, bucket string) error {
	return fmt.Errorf("%w (in s3://%s)", err, bucket)
}

// s3Settings are what a client of the S3 API depends on. Scrapes with the
// same settings share one, and with it the credentials the SDK's chain
// found and keeps until they expire.
type s3Settings struct {
	region, endpoint                                       string
	pathStyle                                              bool
	accessKeyIDFile, secretAccessKeyFile, sessionTokenFile string
}

func s3SettingsOf(c *model.Collector) s3Settings {
	r := &c.Request
	return s3Settings{
		region: r.Region, endpoint: r.Endpoint, pathStyle: r.PathStyle,
		accessKeyIDFile: r.AccessKeyIDFile, secretAccessKeyFile: r.SecretAccessKeyFile, sessionTokenFile: r.SessionTokenFile,
	}
}

// s3ClientCache holds a client for each settings a collector in force has.
type s3ClientCache struct {
	mu      sync.Mutex
	clients map[s3Settings]*s3.Client
}

var s3Clients = &s3ClientCache{clients: map[s3Settings]*s3.Client{}}

// client is the client of settings, made when there is none.
func (cache *s3ClientCache) client(ctx context.Context, settings s3Settings) (*s3.Client, error) {
	cache.mu.Lock()
	client := cache.clients[settings]
	cache.mu.Unlock()
	if client != nil {
		return client, nil
	}
	var options []func(*config.LoadOptions) error
	if settings.region != "" {
		options = append(options, config.WithRegion(settings.region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("loading the AWS SDK's configuration: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = s3DefaultRegion
	}
	client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		if settings.endpoint != "" {
			o.BaseEndpoint = aws.String(settings.endpoint)
		}
		o.UsePathStyle = settings.pathStyle
		o.Retryer = aws.NopRetryer{}
		if settings.accessKeyIDFile != "" {
			// Set on the client rather than given to the configuration,
			// which would keep what it first read for good: the files are
			// read at every request, so a rotated key is used at once.
			o.Credentials = s3FileCredentials{settings.accessKeyIDFile, settings.secretAccessKeyFile, settings.sessionTokenFile}
		}
	})
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if made := cache.clients[settings]; made != nil {
		return made, nil
	}
	cache.clients[settings] = client
	return client, nil
}

// follow drops the clients of settings no collector in force has any more.
func (cache *s3ClientCache) follow(collectors []model.Collector, _ []model.StaticTarget) {
	var inForce []s3Settings
	for i := range collectors {
		if collectors[i].Request.Type == RequestTypeS3File {
			inForce = append(inForce, s3SettingsOf(&collectors[i]))
		}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for settings := range cache.clients {
		if !slices.Contains(inForce, settings) {
			delete(cache.clients, settings)
		}
	}
}

// s3FileCredentials are static credentials read from their files at every
// request.
type s3FileCredentials struct {
	accessKeyIDFile, secretAccessKeyFile, sessionTokenFile string
}

func (f s3FileCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	var values [3]string
	for i, key := range []struct{ name, file string }{{"request.access_key_id_file", f.accessKeyIDFile}, {"request.secret_access_key_file", f.secretAccessKeyFile}, {"request.session_token_file", f.sessionTokenFile}} {
		if key.file == "" {
			continue
		}
		value, err := ReadCredentialFile(key.file)
		if err != nil {
			return aws.Credentials{}, fmt.Errorf("reading %s: %w", key.name, err)
		}
		if value == "" {
			return aws.Credentials{}, fmt.Errorf("%s %s is empty", key.name, key.file)
		}
		values[i] = value
	}
	return aws.Credentials{AccessKeyID: values[0], SecretAccessKey: values[1], SessionToken: values[2], Source: "request.access_key_id_file"}, nil
}

// checkS3Files reads a collector's static credential files as a request
// reads them, for --dry-run: a file missing, unreadable or empty. Loading
// the configuration reads none of them, since a file may be mounted after
// the exporter starts. The default chain is not asked: what it finds
// depends on where the exporter runs, which a dry run need not be.
func checkS3Files(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil || c.Request.AccessKeyIDFile == "" {
		return nil
	}
	s := s3SettingsOf(c)
	if _, err := (s3FileCredentials{s.accessKeyIDFile, s.secretAccessKeyFile, s.sessionTokenFile}).Retrieve(context.Background()); err != nil {
		return []string{fmt.Sprintf("collector %q: %v; a scrape would fail on it now (the file is read at every request, so one put in place later is used then)", c.Name, err)}
	}
	return nil
}

// s3FileSchemaRule is what an s3file collector's request requires, as the
// exporter does (validateS3FileRequest): root, written with something
// other than ""; access_key_id_file and secret_access_key_file together, if
// at all; and session_token_file only beside them. "" is a key left out to
// the exporter, so each is "written" when it is something else (SchemaWrittenKey).
func s3FileSchemaRule() map[string]any {
	rule := SchemaWrittenKey("root")
	rule["allOf"] = []any{
		map[string]any{"if": SchemaWrittenKey("access_key_id_file"), "then": SchemaWrittenKey("secret_access_key_file")},
		map[string]any{"if": SchemaWrittenKey("secret_access_key_file"), "then": SchemaWrittenKey("access_key_id_file")},
		map[string]any{"if": SchemaWrittenKey("session_token_file"), "then": SchemaWrittenKey("access_key_id_file")},
	}
	return rule
}
