package repository

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The exporter's release once published its image and archives unsigned,
// with no SBOM and only buildx's minimal provenance, so a user had nothing
// but the registry's and GitHub's word for what they downloaded. These tests
// hold release.yml to signing the image by the digest it pushed and the
// checksums file that vouches for the archives, keyless with the job's OIDC
// identity, to attaching an SBOM and the full provenance to the image, and
// to doing so in the order the release publishes in.

// The image is pushed with an SBOM and the full provenance, and signed by
// the digest the push reported, after the push and before the Release and
// the move of latest and MAJOR.MINOR, never by a tag; the signature is
// verified before anything else is published.
func TestTheExporterReleaseSignsTheImageByDigestBeforeLatestMoves(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/release.yml")["release"]
	push := slices.IndexFunc(steps, func(step map[string]any) bool {
		_, pushes := buildPushAction(step)
		return pushes
	})
	if push < 0 {
		t.Fatal("release.yml has no step pushing the image")
	}
	with, _ := steps[push]["with"].(map[string]any)
	if with["sbom"] != true || with["provenance"] != "mode=max" {
		t.Errorf("release.yml pushes the image with sbom %v and provenance %v, not an SBOM (true) and the full provenance (mode=max)", with["sbom"], with["provenance"])
	}
	sign := stepIndex(steps, `cosign sign --yes "${IMAGE_NAME}@${DIGEST}"`)
	if sign < 0 {
		t.Fatal("release.yml does not sign the image by its digest with cosign sign --yes")
	}
	run := steps[sign]["run"].(string)
	if env, _ := steps[sign]["env"].(map[string]any); env["DIGEST"] != "${{ steps."+stepID(steps[push])+".outputs.digest }}" {
		t.Errorf("release.yml signs a digest other than the one its push step reported: %v", env["DIGEST"])
	}
	if strings.Contains(run, "--key") || strings.Contains(run, ":${") {
		t.Errorf("release.yml signs the image with a key, or by a tag, not keyless by its digest:\n%s", run)
	}
	if !strings.Contains(run, `cosign verify "${IMAGE_NAME}@${DIGEST}"`) || !strings.Contains(run, "--certificate-oidc-issuer https://token.actions.githubusercontent.com") || !strings.Contains(run, "/.github/workflows/release.yml@${GITHUB_REF}") {
		t.Errorf("release.yml does not verify the image's signature against its workflow's identity:\n%s", run)
	}
	release := stepIndex(steps, "gh release create")
	move := stepIndex(steps, "docker buildx imagetools create")
	if push >= sign || sign >= release || sign >= move {
		t.Errorf("release.yml signs the image in its step %d, which is not after its push (step %d) and before the Release (step %d) and the move of latest and MAJOR.MINOR (step %d)", sign+1, push+1, release+1, move+1)
	}
	if installer := stepUsing(steps, "sigstore/cosign-installer@"); installer < 0 || installer > sign {
		t.Error("release.yml does not install cosign before signing the image")
	}
}

// The checksums file, which names every archive's SHA-256, is signed into a
// Sigstore bundle once it is built and before the Release, and the bundle
// is published with the archives, both when the Release is created and when
// a rerun replaces its assets.
func TestTheExporterReleaseSignsTheChecksumsAndPublishesTheBundle(t *testing.T) {
	steps := workflowSteps(t, ".github/workflows/release.yml")["release"]
	const sums = `sums="dist/prometheus-universal-exporter-${VERSION}-sha256sums.txt"`
	sign := stepIndex(steps, sums, `cosign sign-blob --yes --bundle "${sums}.sigstore.json" "$sums"`, `cosign verify-blob --bundle "${sums}.sigstore.json"`, "--certificate-oidc-issuer https://token.actions.githubusercontent.com")
	if sign < 0 {
		t.Fatal("release.yml does not sign the checksums file into a bundle with cosign sign-blob, and verify it")
	}
	if env, _ := steps[sign]["env"].(map[string]any); env["VERSION"] != "${{ steps.version.outputs.version }}" {
		t.Errorf("release.yml signs the checksums of the version %v", env["VERSION"])
	}
	build := stepIndex(steps, "sha256sum -c")
	release := stepIndex(steps, "gh release create")
	if build >= sign || sign >= release {
		t.Errorf("release.yml signs the checksums in its step %d, not after they are built (step %d) and before the Release (step %d)", sign+1, build+1, release+1)
	}
	run := steps[release]["run"].(string)
	const bundle = `"dist/prometheus-universal-exporter-${VERSION}-sha256sums.txt.sigstore.json"`
	for _, command := range []string{"gh release create", "gh release upload"} {
		found := false
		for line := range strings.Lines(run) {
			if strings.Contains(line, command) && strings.Contains(line, bundle) {
				found = true
			}
		}
		if !found {
			t.Errorf("release.yml's %s does not publish the checksums' bundle %s:\n%s", command, bundle, run)
		}
	}
}

// id-token: write, which lets a job sign with the workflow's identity, is
// granted to exactly the jobs that run cosign, in each release workflow,
// and never at a workflow's top.
func TestOnlyTheJobsThatSignAreGrantedAnIDToken(t *testing.T) {
	for _, path := range []string{".github/workflows/release.yml", ".github/workflows/release-chart.yml"} {
		document := workflowDocument(t, path)
		if top, _ := document["permissions"].(map[string]any); top["id-token"] != nil {
			t.Errorf("%s grants id-token at its top, to every job", path)
		}
		jobs, _ := document["jobs"].(map[string]any)
		signers := 0
		for job, steps := range workflowSteps(t, path) {
			definition, _ := jobs[job].(map[string]any)
			grants, _ := definition["permissions"].(map[string]any)
			signs := stepIndex(steps, "cosign sign") >= 0
			if signs {
				signers++
			}
			if granted := grants["id-token"] == "write"; granted != signs {
				t.Errorf("%s job %s is granted id-token %v, and signs: %v", filepath.Base(path), job, grants["id-token"], signs)
			}
		}
		if signers != 1 {
			t.Errorf("%s has %d jobs that sign, not one", path, signers)
		}
	}
}
