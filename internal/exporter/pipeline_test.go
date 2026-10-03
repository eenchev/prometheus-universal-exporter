package exporter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A probe and a static target's scrape make their trip through collect
// alone (pipeline.go), so the two cannot drift apart again. Only a
// directory's files are decoded and transformed elsewhere (filebatch.go),
// one at a time, from inside collect.
func TestProbesAndStaticTargetsShareOnePipeline(t *testing.T) {
	allowed := map[string]map[string]bool{
		"fetch.FetchCollector(": {"pipeline.go": true},
		"decode.Decode(":        {"pipeline.go": true, "filebatch.go": true},
		"transform.Transform(":  {"pipeline.go": true, "filebatch.go": true},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for call, where := range allowed {
			if strings.Contains(string(source), call) && !where[file] {
				t.Errorf("%s calls %s; a trip to the target belongs in collect (pipeline.go)", file, call)
			}
		}
	}
}

func TestBodyExcerpt(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes
	for body, want := range map[string]string{
		"":                       "",
		"  \n\t ":                "",
		"quota exceeded\r\n":     "quota exceeded",
		"bad\x00\x01bytes\xff!":  "bad bytes !",
		long:                     strings.Repeat("é", 128) + "…",
		strings.Repeat("a", 300): strings.Repeat("a", 256) + "…",
	} {
		if got := bodyExcerpt([]byte(body)); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}

// An error page that echoes a credential does not put it in the log, even
// where the excerpt's cut runs through it.
func TestBodyExcerptMasksCredentials(t *testing.T) {
	for body, secret := range map[string]string{
		`{"error":"invalid_grant","refresh_token":"s3cretAAAA"}`: "s3cretAAAA",
		`<p>Bearer s3cretBBBBBBBB was revoked</p>`:               "s3cretBBBBBBBB",
		strings.Repeat("x", 245) + " token=s3cretCCCCCCCCCCCC":   "s3cret",
	} {
		got := bodyExcerpt([]byte(body))
		if strings.Contains(got, secret) || !strings.Contains(got, "<red") {
			t.Errorf("%q: %q", body, got)
		}
	}
}
