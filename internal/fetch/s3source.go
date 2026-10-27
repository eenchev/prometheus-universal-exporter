//go:build request_type_s3file

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An s3file read reads through the S3 API what localfile reads through an
// os.Root (filereads.go): s3Source is the fileSource of request.root in a
// bucket. A bucket has no directories, only keys, so root is a key prefix
// read as a directory: root /exports/app is the keys that start with
// exports/app/, and the file app.prom under it the key exports/app/app.prom.
// Every name is checked lexically before it is joined onto the prefix, as
// localfile's are, and there are no links to follow, so the lexical check
// is the whole confinement.
//
// An object is a regular file: its size is its Content-Length, its
// modification time its Last-Modified. Stat is a HeadObject, OpenFile a
// GetObject and a directory's listing ListObjectsV2 with the delimiter /,
// whose common prefixes are the directory's subdirectories. A listing
// describes every object it lists, so the Stat that reading a directory
// asks of each is answered from it, rather than with a HeadObject apiece,
// until a GetObject finds the object changed since (OpenFile).

// s3Source is request.root in a bucket, through the S3 API.
type s3Source struct {
	// ctx is the scrape's: the fileSource methods take none, and every
	// request a read makes is made within the scrape's deadline.
	ctx    context.Context
	client *s3.Client
	bucket string
	// prefix is root as a key prefix: "" for /, and otherwise root without
	// its leading /, ending in one.
	prefix string
	// listed are the objects a listing described, by name under root.
	mu     sync.Mutex
	listed map[string]s3Info
}

// s3Prefix is request.root as the key prefix of the objects under it.
func s3Prefix(root string) string {
	if p := strings.Trim(root, "/"); p != "" {
		return p + "/"
	}
	return ""
}

// key is the object key of name, a name under root that the read has
// checked lexically already (fileUnderRoot); "." is root itself.
func (s *s3Source) key(name string) string {
	if name = path.Clean(name); name == "." {
		return s.prefix
	}
	return s.prefix + name
}

// s3Info describes an object, or a directory, a common prefix of a listing.
type s3Info struct {
	name     string
	size     int64
	modified time.Time
	etag     string
	dir      bool
}

func (i s3Info) Name() string { return i.name }
func (i s3Info) Size() int64  { return i.size }
func (i s3Info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i s3Info) ModTime() time.Time { return i.modified }
func (i s3Info) IsDir() bool        { return i.dir }
func (i s3Info) Sys() any           { return nil }

// objectInfo is the s3Info of an object from what an answer says of it.
func objectInfo(name string, size *int64, modified *time.Time, etag *string) s3Info {
	return s3Info{name: path.Base(name), size: aws.ToInt64(size), modified: aws.ToTime(modified), etag: aws.ToString(etag)}
}

func (s *s3Source) Stat(name string) (fs.FileInfo, error) {
	s.mu.Lock()
	info, listed := s.listed[path.Clean(name)]
	s.mu.Unlock()
	if listed {
		return info, nil
	}
	out, err := s.client.HeadObject(s.ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(name))})
	if err != nil {
		err = s3Error(err)
		// The answer to a HeadObject has no body, so a missing bucket and a
		// missing object are one 404 to it: the bucket is asked apart,
		// once the object is found missing.
		if errors.Is(err, fs.ErrNotExist) {
			if _, bucketErr := s.client.HeadBucket(s.ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); errors.Is(s3Error(bucketErr), fs.ErrNotExist) {
				return nil, fmt.Errorf("the bucket does not exist: %w", s3Told(bucketErr))
			}
		}
		return nil, err
	}
	return objectInfo(name, out.ContentLength, out.LastModified, out.ETag), nil
}

func (s *s3Source) OpenFile(name string) (sourceFile, error) {
	out, err := s.client.GetObject(s.ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(name))})
	if err != nil {
		return nil, s3Error(err)
	}
	info := objectInfo(name, out.ContentLength, out.LastModified, out.ETag)
	// An object replaced since the listing described it is described by its
	// own answers from now on, so the read tried again (readSourceFile)
	// compares the object with itself, not with the listing.
	s.mu.Lock()
	if was, listed := s.listed[path.Clean(name)]; listed && (was.size != info.size || !was.modified.Equal(info.modified)) {
		delete(s.listed, path.Clean(name))
	}
	s.mu.Unlock()
	return &s3File{body: out.Body, info: info}, nil
}

func (s *s3Source) OpenDir(name string) (sourceDir, error) {
	prefix := s.key(name)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &s3Dir{src: s, dir: path.Clean(name), prefix: prefix}, nil
}

// s3File is an object being read: the body of its GetObject, and what the
// answer said of it.
type s3File struct {
	body io.ReadCloser
	info s3Info
}

func (f *s3File) Read(p []byte) (int, error) { return f.body.Read(p) }
func (f *s3File) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *s3File) Close() error               { return f.body.Close() }

// s3EmptyPages is how many pages in a row a listing may answer with no
// entry while it says more follow, before it is refused: a store that does
// so forever would otherwise hold the scrape to its deadline.
const s3EmptyPages = 16

// s3Dir lists the objects directly under a prefix, a page at a time as the
// directory read asks for entries, so the bound on the entries a scrape
// lists (maxListedEntries) is the bound on what is asked of the store.
type s3Dir struct {
	src    *s3Source
	dir    string
	prefix string
	token  *string
	done   bool
}

// Stat describes the directory. A prefix exists whether or not any key has
// it, so it is always a directory, empty when nothing is under it.
func (d *s3Dir) Stat() (fs.FileInfo, error) {
	return s3Info{name: path.Base(d.dir), dir: true}, nil
}

func (d *s3Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	for empty := 0; !d.done; empty++ {
		if empty == s3EmptyPages {
			return nil, fmt.Errorf("the store answered %d requests for the objects under %q in a row with none while saying more follow", s3EmptyPages, d.prefix)
		}
		out, err := d.src.client.ListObjectsV2(d.src.ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(d.src.bucket),
			Prefix:            aws.String(d.prefix),
			Delimiter:         aws.String("/"),
			MaxKeys:           aws.Int32(int32(min(max(n, 1), 1000))), // #nosec G115 -- bounded to 1..1000
			ContinuationToken: d.token,
		})
		if err != nil {
			return nil, s3Error(err)
		}
		d.token = out.NextContinuationToken
		d.done = !aws.ToBool(out.IsTruncated) || d.token == nil
		var entries []fs.DirEntry
		for _, object := range out.Contents {
			base, ok := strings.CutPrefix(aws.ToString(object.Key), d.prefix)
			// The key of the prefix itself is the marker a console leaves
			// for a folder, no file of it.
			if !ok || base == "" || strings.Contains(base, "/") {
				continue
			}
			info := objectInfo(base, object.Size, object.LastModified, object.ETag)
			d.src.mu.Lock()
			if d.src.listed == nil {
				d.src.listed = map[string]s3Info{}
			}
			d.src.listed[path.Join(d.dir, base)] = info
			d.src.mu.Unlock()
			entries = append(entries, fs.FileInfoToDirEntry(info))
		}
		for _, common := range out.CommonPrefixes {
			base, ok := strings.CutPrefix(strings.TrimSuffix(aws.ToString(common.Prefix), "/"), d.prefix)
			if ok && base != "" {
				entries = append(entries, fs.FileInfoToDirEntry(s3Info{name: base, dir: true}))
			}
		}
		if len(entries) > 0 {
			return entries, nil
		}
	}
	return nil, io.EOF
}

func (d *s3Dir) Close() error { return nil }

// s3Told is err, a failed request's error, marked as the operator's
// (model.WithOperatorDetail) when the store answered it or the SDK found no
// credentials: the store's message names the account and the principal the
// request was made as (`User: arn:aws:iam::123456789012:role/exporter is
// not authorized ...`), and the SDK's credential chain the sources it tried,
// their files and endpoints. A probe's caller is told the operation, the
// status and the store's error code, or that no credentials were found,
// and that the rest is in the exporter's log. A failure of the network is
// left as it is.
func s3Told(err error) error {
	if err == nil {
		return nil
	}
	operation := "the request"
	var op *smithy.OperationError
	if errors.As(err, &op) {
		operation = "operation error " + op.Service() + ": " + op.Operation()
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		answered := "the store answered"
		var status interface{ HTTPStatusCode() int }
		if errors.As(err, &status) {
			answered += " " + strconv.Itoa(status.HTTPStatusCode())
		}
		return model.WithOperatorDetail(err, operation+", "+answered+" "+api.ErrorCode()+"; its message is in the exporter's log")
	}
	// The SDK's words for an identity it could not get, before the chain's.
	if op != nil && strings.Contains(err.Error(), "get identity: ") {
		return model.WithOperatorDetail(err, operation+", the AWS SDK found no credentials it could use; which it tried is in the exporter's log")
	}
	return err
}

// s3Error is a failed request's error as the file reads word theirs
// (localFileError): an object that is not there is fs.ErrNotExist, and an
// answer of 403 says what the credentials lack. Anything else is the SDK's
// error, which names the operation, the status and the store's own code.
func s3Error(err error) error {
	err = s3Told(err)
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "NoSuchBucket" {
		return fmt.Errorf("the bucket does not exist: %w", err)
	}
	var status interface{ HTTPStatusCode() int }
	if !errors.As(err, &status) {
		return err
	}
	switch status.HTTPStatusCode() {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	case http.StatusForbidden:
		return fmt.Errorf("access denied (403): the credentials need s3:GetObject on the object, and s3:ListBucket on the bucket for a missing object to be told apart and for a directory to be listed: %w", err)
	}
	return err
}
