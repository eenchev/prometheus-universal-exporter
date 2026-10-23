package repository

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The exporter's release once pushed the image, latest and MAJOR.MINOR
// among its tags, before it cross-compiled the archives and created the
// GitHub Release: a compile error of darwin alone, or a Release left from an
// earlier run, failed the release with latest already moved to an image no
// Release went with. These tests hold release.yml to building everything
// before publishing anything, and to the order it publishes in.

// buildPushAction reports whether a step runs docker/build-push-action, and
// whether that step pushes.
func buildPushAction(step map[string]any) (uses, pushes bool) {
	name, _ := step["uses"].(string)
	if !strings.HasPrefix(name, "docker/build-push-action@") {
		return false, false
	}
	with, _ := step["with"].(map[string]any)
	return true, with["push"] != false
}

// releasePublishes reports whether a step of the release puts something out
// a user can see: an image pushed or tagged, or a GitHub Release.
func releasePublishes(step map[string]any) bool {
	if _, pushes := buildPushAction(step); pushes {
		return true
	}
	run, _ := step["run"].(string)
	return strings.Contains(run, "gh release") || strings.Contains(run, "imagetools create") || strings.Contains(run, "docker push")
}

// Every archive, darwin's among them, their checksums and the image are
// built before the first step that publishes; the image goes out under its
// version alone; the Release is created as a draft, or found and its assets
// replaced on a rerun, and published; and latest and MAJOR.MINOR move last.
func TestTheExporterReleasePublishesNothingBeforeEverythingIsBuilt(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/release.yml")["release"]
	archives := stepIndex(steps, "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "tar -czf", "sha256sum -c")
	if archives < 0 {
		t.Fatal("release.yml has no step building the archives of every platform and their checksums")
	}
	image := slices.IndexFunc(steps, func(step map[string]any) bool {
		uses, pushes := buildPushAction(step)
		return uses && !pushes
	})
	if image < 0 {
		t.Fatal("release.yml builds no image without pushing it, so the image's build is not known to succeed before something is published")
	}
	published := 0
	for index, step := range steps {
		if !releasePublishes(step) {
			continue
		}
		published++
		if index < archives || index < image {
			t.Errorf("release.yml publishes before the archives and the image are built, in its step %d:\n%s", index+1, yamlText(step))
		}
	}
	push := slices.IndexFunc(steps, func(step map[string]any) bool {
		_, pushes := buildPushAction(step)
		return pushes
	})
	release := stepIndex(steps, "gh release view", "gh release upload", "--clobber", "gh release create", "--draft", "gh release edit", "--draft=false")
	move := stepIndex(steps, "docker buildx imagetools create", "latest", "${MAJOR_MINOR}", "${DIGEST}")
	if push < 0 || release < 0 || move < 0 || published != 3 {
		t.Fatalf("release.yml was meant to publish in three steps, the image's push, the Release's (created as a draft, or its assets replaced when it exists, then published) and the move of latest and MAJOR.MINOR; %d steps publish, and the three were found at %d, %d and %d", published, push, release, move)
	}
	if push >= release || release >= move {
		t.Errorf("release.yml publishes the image (step %d), the Release (step %d) and moves latest and MAJOR.MINOR (step %d) out of that order", push+1, release+1, move+1)
	}
	if env, _ := steps[move]["env"].(map[string]any); env["DIGEST"] != "${{ steps."+stepID(steps[push])+".outputs.digest }}" {
		t.Errorf("release.yml moves latest and MAJOR.MINOR to something other than the digest its push step reported: %v", env["DIGEST"])
	}
	meta := slices.IndexFunc(steps, func(step map[string]any) bool { return step["id"] == "meta" })
	if meta < 0 {
		t.Fatal("release.yml has no step meta naming the image's tags")
	}
	with, _ := steps[meta]["with"].(map[string]any)
	tags, _ := with["tags"].(string)
	if !strings.Contains(tags, "steps.version.outputs.version") || strings.Contains(tags, "latest") || strings.Contains(tags, "major_minor") {
		t.Errorf("the image is pushed with tags other than its version alone, which move before the Release is out:\n%s", tags)
	}
}

// stepID returns a step's id, or "" for one with none.
func stepID(step map[string]any) string {
	id, _ := step["id"].(string)
	return id
}

// dockerfileLdflags is the -ldflags argument of a go build.
var dockerfileLdflags = regexp.MustCompile(`-ldflags="([^"]*)"`)

// The archives' binaries are linked with the -ldflags the image's are, the
// revision among them, from the same values, so --version reports the same
// whichever a user runs; the archives once had no revision.
func TestTheReleaseArchivesAreLinkedAsTheImageIs(t *testing.T) {
	dockerfile := read(t, "Dockerfile")
	var want []string
	for _, match := range dockerfileLdflags.FindAllStringSubmatch(dockerfile, -1) {
		want = append(want, match[1])
	}
	if len(want) != 2 || want[0] != want[1] || !strings.Contains(want[0], "-X main.revision=${REVISION}") {
		t.Fatalf("the Dockerfile's two go builds were meant to pass the same -ldflags with the revision; they pass %q", want)
	}
	steps := workflowSteps(t, ".github/workflows/release.yml")["release"]
	archives := stepIndex(steps, "tar -czf")
	if archives < 0 {
		t.Fatal("release.yml has no step building the archives")
	}
	run, _ := steps[archives]["run"].(string)
	builds := 0
	for line := range strings.Lines(run) {
		if !strings.Contains(line, "go build") {
			continue
		}
		builds++
		match := dockerfileLdflags.FindStringSubmatch(line)
		if match == nil || match[1] != want[0] {
			t.Errorf("an archive's binary is linked with other -ldflags than the image's %q:\n%s", want[0], strings.TrimSpace(line))
		}
	}
	if builds != 2 {
		t.Errorf("release.yml's archive step runs %d go builds, not the exporter's and the allocator's", builds)
	}
	// The values are those the image's build is given.
	env, _ := steps[archives]["env"].(map[string]any)
	for name, value := range map[string]string{"VERSION": "${{ steps.version.outputs.version }}", "REVISION": "${{ github.sha }}"} {
		if env[name] != value {
			t.Errorf("release.yml's archive step sets %s to %v, where the image is given %s", name, env[name], value)
		}
	}
}

// CI builds what the release builds, so a change that breaks it fails on
// its pull request: the code for darwin, whose archives the release
// cross-compiles, and the image from the Dockerfile, pushed nowhere. Both
// run under the go filter, which matches every file they read.
func TestCIBuildsForMacOSAndBuildsTheImage(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/ci.yml")["test"]
	for _, c := range []struct {
		what      string
		fragments []string
	}{
		{"builds for darwin", []string{"GOOS=darwin", "amd64", "arm64", "go build ./...", "-o /dev/null .", "-o /dev/null ./cmd/pue-allocator"}},
		{"builds the image", []string{"docker build", "REVISION=", "--version"}},
	} {
		index := stepIndex(steps, c.fragments...)
		if index < 0 {
			t.Errorf("no step of ci.yml's job test %s (%q)", c.what, c.fragments)
			continue
		}
		if steps[index]["if"] != sideCondition("go") {
			t.Errorf("the step of ci.yml that %s runs under %v, not the go filter", c.what, steps[index]["if"])
		}
		if releasePublishes(steps[index]) || strings.Contains(steps[index]["run"].(string), "--push") {
			t.Errorf("the step of ci.yml that %s publishes something", c.what)
		}
	}
	patterns := ciFilters(t)["go"]
	for _, file := range []string{"Dockerfile", ".dockerignore", "tools/request-type-tags.sh", "go.mod", "cmd/pue-allocator/main.go"} {
		if !matchesAny(patterns, file) {
			t.Errorf("ci.yml's go filter does not match %s, which the image's build reads", file)
		}
	}
}
