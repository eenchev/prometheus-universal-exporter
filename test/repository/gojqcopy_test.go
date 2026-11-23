package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// gojqCopy is where the copy of gojq that go.mod replaces the module with
// is, and gojqOwn the files there that are the exporter's and not gojq's:
// its one new source, what says how the copy is made, and the two files of
// a module that builds by itself.
const gojqCopy = "third_party/gojq"

var gojqOwn = []string{"PATCH.md", "UPSTREAM.sha256", "go.mod", "go.sum", "gojq.patch", "maxvalue.go"}

// gojqUnpatched is the text of a file of the copy with gojq.patch taken out
// of it: each of the patch's hunks for the file found once, as it is after
// the patch, and written as it was before.
func gojqUnpatched(t *testing.T, name, text, patch string) string {
	t.Helper()
	_, hunks, patched := strings.Cut(patch, "--- a/"+name+"\n+++ b/"+name+"\n")
	if !patched {
		return text
	}
	if next := strings.Index(hunks, "\n--- a/"); next >= 0 {
		hunks = hunks[:next+1]
	}
	for i, hunk := range strings.Split(hunks, "\n@@ ") {
		if i == 0 {
			hunk = strings.TrimPrefix(hunk, "@@ ")
		}
		at, body, _ := strings.Cut(hunk, "\n")
		var before, after strings.Builder
		for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "+"):
				after.WriteString(line[1:] + "\n")
			case strings.HasPrefix(line, "-"):
				before.WriteString(line[1:] + "\n")
			default:
				before.WriteString(strings.TrimPrefix(line, " ") + "\n")
				after.WriteString(strings.TrimPrefix(line, " ") + "\n")
			}
		}
		if n := strings.Count(text, after.String()); n != 1 {
			t.Fatalf("%s holds %d times what gojq.patch leaves at @@ %s, want once; the copy and the patch have parted", name, n, at)
		}
		text = strings.Replace(text, after.String(), before.String(), 1)
	}
	return text
}

// The copy of gojq is the release go.mod names with gojq.patch applied and
// nothing else: every file of it has the checksum upstream's has
// (UPSTREAM.sha256) once the patch is taken out, there is no file of gojq's
// the list does not hold and none of the list's is missing. So a change
// made in the copy by hand, which no update of gojq would carry over and no
// reader of PATCH.md would know of, fails here, and so does a copy moved to
// another release with the version or the checksums left as they were.
func TestTheCopyOfGojqIsUpstreamsWithThePatchAndNothingElse(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(gojqCopy, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	manifest, patch := read("UPSTREAM.sha256"), read("gojq.patch")
	released := regexp.MustCompile(`(?m)^# gojq (v\S+):`).FindStringSubmatch(manifest)
	if released == nil {
		t.Fatal("UPSTREAM.sha256 does not begin by naming the release, as # gojq v0.12.20:")
	}
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if required := regexp.MustCompile(`(?m)^\tgithub\.com/itchyny/gojq (v\S+)`).FindSubmatch(module); required == nil || string(required[1]) != released[1] {
		t.Errorf("go.mod requires gojq %s and the copy is of %s", required, released[1])
	}
	if !strings.Contains(string(module), "\nreplace github.com/itchyny/gojq => ./"+gojqCopy+"\n") {
		t.Errorf("go.mod does not replace gojq with ./%s, so the exporter is built from another gojq than the one patched", gojqCopy)
	}

	sums := map[string]string{}
	for line := range strings.SplitSeq(manifest, "\n") {
		if sum, name, listed := strings.Cut(line, "  "); listed && !strings.HasPrefix(line, "#") {
			sums[name] = sum
		}
	}
	entries, err := os.ReadDir(gojqCopy)
	if err != nil {
		t.Fatal(err)
	}
	var present []string
	for _, entry := range entries {
		present = append(present, entry.Name())
		if _, listed := sums[entry.Name()]; !listed && !slices.Contains(gojqOwn, entry.Name()) {
			t.Errorf("%s/%s is neither a file of gojq's that UPSTREAM.sha256 lists nor one of the exporter's own there", gojqCopy, entry.Name())
		}
	}
	for _, name := range gojqOwn {
		if !slices.Contains(present, name) {
			t.Errorf("%s/%s is missing", gojqCopy, name)
		}
	}
	patchedFiles := 0
	for name, want := range sums {
		if !slices.Contains(present, name) {
			t.Errorf("%s/%s, which UPSTREAM.sha256 lists, is missing", gojqCopy, name)
			continue
		}
		text := read(name)
		unpatched := gojqUnpatched(t, name, text, patch)
		if unpatched != text {
			patchedFiles++
		}
		sum := sha256.Sum256([]byte(unpatched))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s/%s is not the file gojq %s has, with gojq.patch taken out: its SHA-256 is %s and UPSTREAM.sha256 says %s. A change to gojq goes into gojq.patch and PATCH.md, and a move to another release follows PATCH.md", gojqCopy, name, released[1], got, want)
		}
	}
	if changed := strings.Count(patch, "+++ b/"); changed == 0 || patchedFiles != changed {
		t.Errorf("gojq.patch changes %d files, and %d of the copy's hold what it leaves", changed, patchedFiles)
	}
}

// What keeps the copy of gojq built and looked after is in place: the image
// has the copy's go.mod before it downloads the modules, which reads it;
// Dependabot is told to leave the module, whose version in go.mod only
// names what was copied; and a workflow looks for a newer release, which
// nothing else would say there is.
func TestTheCopyOfGojqIsBuiltAndLookedAfter(t *testing.T) {
	for file, want := range map[string]string{
		"Dockerfile":                         "COPY " + gojqCopy + "/go.mod " + gojqCopy + "/go.mod\nRUN go mod download\n",
		".github/dependabot.yml":             `- dependency-name: "github.com/itchyny/gojq"`,
		".github/workflows/gojq-release.yml": "repos/itchyny/gojq/releases/latest",
		"Makefile":                           "git ls-files '*.go' ':!third_party'",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Error(err)
			continue
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s no longer has %q", file, want)
		}
	}
}
