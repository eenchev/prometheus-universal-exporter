# Releases

The exporter and the Helm chart are released independently, each from its own
tag and workflow. A chart fix does not require an exporter release, and an
exporter release does not republish the chart.

## CI has to pass first

Neither release publishes anything unless CI has passed on the commit the tag
names. The first job of each workflow, `preflight`, checks the tag's format
and then runs `tools/ci-passed.py`, which reads the commit's `ci.yml` runs
through the GitHub API. CI has passed when the newest finished run that a push
of that commit started, and that ran the suite, concluded `success`. A run
that failed counts whatever it ran; a run that succeeded counts only when its
step *Run the repository's checks* did, since a push whose changed paths ran
none of the Go steps passes with every step skipped and says nothing about
the commit — so it takes back no failure before it. Runs started by a pull
request do not count, since they tested a merge rather than the commit.

A tag push runs every step of CI whatever changed: the changed-path filter
compares a tag with its merge base on `main`, which for a tag on `main`'s
newest commit is the commit itself, so it would find no change and skip
everything. The step `changes` after the filter turns each of its outputs
true on a tag push, and every step runs under those.

CI runs on a push to `main`, on a push of any tag and on a pull request; a
push to another branch runs nothing of its own, its pull request's run being
the one that tests it. A release tag on a commit that is not on `main` is
still tested, by the run its own push starts.

Pushing the tag starts a CI run of its own, and a tag is often pushed right
after its commit, so while the commit's runs are queued or running, or none
has started yet, the job waits, for up to an hour, looking every 30 seconds.
When every run has finished and the newest counted one did not succeed, or
none ran the suite, or the hour runs out, the release stops with `CI has not passed on <sha>: run CI or wait for it,
then push the tag again or re-run this release`, before anything is built or
pushed. To release that commit once CI is green — after re-running a failed CI
run, say — re-run the failed release run from its page under Actions (*Re-run
all jobs*), or delete the tag and push it again:

```sh
git push --delete origin exporter/prometheus-universal-exporter-v1.0.0
git push origin exporter/prometheus-universal-exporter-v1.0.0
```

When CI fails on the commit for a reason of the commit's own, fix it in a new
commit and tag that one instead.

## Release notes

Each GitHub Release's description is its section of
[`CHANGELOG.md`](../CHANGELOG.md), not a list of the pull requests merged
since the last tag: a user upgrading needs to know what to do, and a pull
request's title rarely says. The exporter's sections are headed
`## exporter MAJOR.MINOR.PATCH` and the chart's `## chart MAJOR.MINOR.PATCH`,
each component's newest first, below one `## Unreleased` at the top. Every
section begins with `### What changed for you`: what someone upgrading has to
do or will notice — a configuration key or chart value renamed or removed, a
default changed, a metric renamed, a request refused that was accepted — or
"Nothing; you can upgrade without changes." when there is nothing. `### New`
and `### Fixed` follow, each left out when it would be empty; a section has
no other subsections.

Every pull request adds its lines to `## Unreleased`, under the subsection
they belong in, replacing the "Nothing" line when it adds the first line to
`### What changed for you`. Lines are written for the people who run the
exporter or install the chart: an internal refactor, a test or a CI change
gets none. A line about the chart alone names the chart value it is about.

Releasing begins with a commit that moves the lines the release ships out of
`## Unreleased` into a new section for the release, under it:
`## exporter 1.1.0` takes the exporter's lines, and `## chart 1.1.0` the lines
about the chart and the exporter release it deploys. Lines the release does
not ship, the chart's at an exporter release, stay under `## Unreleased`,
which is left with "Nothing; you can upgrade without changes." when nothing
stays.

`tools/release-notes.sh exporter|chart VERSION` prints a section's text
without its heading, and both release workflows run it, once the tag's
version is known and before they build or check anything, passing what it
prints to `gh release` with `--notes-file`. It fails, and the release with
it, when the section is missing or does not begin with
`### What changed for you` and a line under it, saying which; to release
then, add the section in a new commit and tag that one. A test reads
`CHANGELOG.md` and fails on a heading of another shape, versions out of order
or a subsection of another name, and requires a section for the chart version
`Chart.yaml` declares and for the exporter release its `appVersion` names.

## Exporter

An `exporter/prometheus-universal-exporter-vMAJOR.MINOR.PATCH` tag runs
`release.yml`, which, once CI has passed on the tagged commit, runs the test
suite against the Python and libraries the
image ships (the Dockerfile's `PYTHON_VERSION`, `LXML_VERSION`,
`PYYAML_VERSION` and `PYTHON_DATEUTIL_VERSION`, as CI does), builds the
cross-platform archives and the container image, and then publishes them: the
image to GHCR and a GitHub Release containing the archives and
`prometheus-universal-exporter-<version>-sha256sums.txt`, their SHA-256
checksums:

Move the release's lines from `## Unreleased` into `## exporter 1.0.0` in
`CHANGELOG.md` ([Release notes](#release-notes)), commit, push, and wait for
CI; then tag that commit:

```sh
git tag -a exporter/prometheus-universal-exporter-v1.0.0 -m "Exporter v1.0.0"
git push origin exporter/prometheus-universal-exporter-v1.0.0
```

The image is published as `ghcr.io/eenchev/prometheus-universal-exporter:1.0.0`,
also tagged `1.0` when it is the newest release of 1.0, and `latest` when it
is the newest release of all. It holds the target allocator too, as
`/bin/pue-allocator` ([Target allocator](ALLOCATOR.md)). Each archive, one
for each of `linux` and `darwin` on `amd64` and `arm64`, holds the two
binaries, `prometheus-universal-exporter` and `pue-allocator`, and
`config.example.yaml`. A patch to an older line, say 1.0.1 after
1.1.0, is published as `1.0.1` and `1.0`, and leaves `latest` on 1.1.0.

Nothing is published until everything is built, and what is published goes
out in an order that a failure at any step leaves consistent:

1. The archives, for every platform, and their checksums are built, and the
   image is built for `linux/amd64` and `linux/arm64` without being pushed.
   A compile error of one platform alone, or a broken image build, fails the
   release here, with nothing published.
2. The image is pushed under its version tag alone, `1.0.0`, from the build
   above, which it does not repeat, with an SBOM of what it holds and the
   full provenance of its build (`mode=max`) attached as attestations.
3. The image is signed with cosign by the digest the push reported, not by
   its tag, which could be moved between the push and the signature, and
   the checksums file is signed into a Sigstore bundle,
   `prometheus-universal-exporter-1.0.0-sha256sums.txt.sigstore.json`. Both
   signatures are keyless: their certificates name `release.yml` and the tag
   it ran for, and each is verified before the release goes on.
4. The GitHub Release is created as a draft with the archives, the
   checksums file and its bundle, and its description from `CHANGELOG.md`,
   and then published, so it never shows without them.
5. `latest` and `1.0` are moved to the image the version tag names, by its
   digest (`docker buildx imagetools create`), when the release is the newest
   of all or of its line.

A failure at steps 2 to 4 leaves `latest` and `1.0` where they were; a
failure at step 5 leaves a published Release whose image is out under its
version. Re-running the release repeats every step: the image is built and
pushed again under the same tag and signed again, the checksums are signed
again, and the Release, which `gh release create`
would refuse because it exists, is found with `gh release view` and its
assets replaced (`gh release upload --clobber`) and its description set
again from `CHANGELOG.md` (`gh release edit --notes-file`) before it is
published, if it was left a draft. The tags then move as on the first run. The archives'
binaries are linked with the `-ldflags` the image's are, so `--version`
reports the release's version and commit whichever a user runs.

To check a downloaded archive, download the checksums file beside it and run,
on Linux:

```sh
sha256sum --ignore-missing -c prometheus-universal-exporter-1.0.0-sha256sums.txt
```

macOS, which the `darwin` archives are for, has no `sha256sum`; its `shasum`
reads the same file:

```sh
shasum -a 256 --ignore-missing -c prometheus-universal-exporter-1.0.0-sha256sums.txt
```

The checksums are only as good as the file they are read from, so check its
signature first: download the bundle beside it and run

```sh
cosign verify-blob prometheus-universal-exporter-1.0.0-sha256sums.txt \
  --bundle prometheus-universal-exporter-1.0.0-sha256sums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/eenchev/prometheus-universal-exporter/\.github/workflows/release\.yml@refs/tags/exporter/prometheus-universal-exporter-v[0-9]+\.[0-9]+\.[0-9]+$'
```

A file that verifies was signed by the exporter's release workflow for a
release tag, and every archive whose checksum it names is then checked by
`sha256sum -c`.

To verify the image, by its digest or by a tag, which `cosign verify`
resolves to the digest; `latest` and `1.0` point at the digest the version
was signed by, so the one signature covers them:

```sh
cosign verify ghcr.io/eenchev/prometheus-universal-exporter:1.0.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/eenchev/prometheus-universal-exporter/\.github/workflows/release\.yml@refs/tags/exporter/prometheus-universal-exporter-v[0-9]+\.[0-9]+\.[0-9]+$'
```

The image's SBOM and provenance are read from the registry:

```sh
docker buildx imagetools inspect ghcr.io/eenchev/prometheus-universal-exporter:1.0.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/eenchev/prometheus-universal-exporter:1.0.0 --format '{{ json .Provenance }}'
```

## Helm chart

Chart tags are namespaced under `chart/`. Bump `version` in
`charts/prometheus-universal-exporter/Chart.yaml` first — it is the source of
truth, and `release-chart.yml` refuses to publish a tag that disagrees with it.
The documented install commands and the chart README's dependency example pin
the same version, and so does the parent chart the chart checks render
(`testdata/chart/parent/Chart.yaml`); tests fail until they follow, and
until `CHANGELOG.md` has a `## chart 0.2.0` section with the lines moved from
`## Unreleased` ([Release notes](#release-notes)):

```sh
# after setting version: 0.2.0 in Chart.yaml and adding ## chart 0.2.0
git tag -a chart/prometheus-universal-exporter-0.2.0 -m "Chart 0.2.0"
git push origin chart/prometheus-universal-exporter-0.2.0
```

Once CI has passed on the tagged commit, the workflow re-runs the chart lint
and template checks, checks that the image
the chart deploys by default, `ghcr.io/eenchev/prometheus-universal-exporter:<appVersion>`,
exists (`docker buildx imagetools inspect`), packages the chart exactly as
`Chart.yaml` declares it, pushes it to
`oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`, signs it with
cosign by the digest `helm push` reported — not by the tag, which could be
moved between the push and the signature — verifies that signature, and
creates a GitHub Release with the archive and the version's section of
`CHANGELOG.md` as its description. When the image does not exist, the
release fails before anything is pushed, saying to release the exporter first.

To verify a published chart by its digest:

```sh
cosign verify ghcr.io/eenchev/charts/prometheus-universal-exporter@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/eenchev/prometheus-universal-exporter/\.github/workflows/release-chart\.yml@refs/tags/chart/prometheus-universal-exporter-[0-9]+\.[0-9]+\.[0-9]+$'
```

`cosign verify` also accepts the tag, which it resolves to that digest.

`appVersion` in `Chart.yaml` is the exporter release a chart version deploys:
`image.tag` is empty by default, which renders `appVersion`, so installing a
chart version always runs the exporter release it was validated against. It is
maintained by hand, and the Artifact Hub `artifacthub.io/images` annotation
names the same tag; a test fails when they disagree. Release the exporter
first, so the image the chart points at exists when the chart is published;
the chart release checks it and refuses otherwise.

A release of both, say 1.1.0:

1. Move the exporter's lines from `## Unreleased` into `## exporter 1.1.0`
   in `CHANGELOG.md`, commit, push, and wait for CI. Tag and push
   `exporter/prometheus-universal-exporter-v1.1.0`, and wait for
   `release.yml` to publish the image.
2. In `Chart.yaml`, set `version` (the chart's own version) and `appVersion:
   "1.1.0"`, and the `artifacthub.io/images` tag to `1.1.0`. Update the
   `--version` in the `helm install` examples of `README.md` and the chart
   README. Move the chart's lines from `## Unreleased` into
   `## chart <version>`, which says it deploys exporter 1.1.0. Commit, push,
   and wait for CI.
3. Tag and push `chart/prometheus-universal-exporter-<version>`.

Both workflows trigger on every tag in their namespace, not only well-formed
ones, and fail fast on a tag that does not match the required format — a tag
like `exporter/prometheus-universal-exporter-1.0.0` (no `v`) or
`chart/prometheus-universal-exporter-0.2` reports the expected format instead of
matching no workflow and looking like it released.

## GHCR package visibility

A package pushed to GHCR is private until somebody says otherwise, and the first
symptom is a user running `helm install` against a public chart and being asked
to authenticate. Both packages — `prometheus-universal-exporter` (the image) and
`charts/prometheus-universal-exporter` (the chart) — have to be made public
once, in the repository's package settings on GitHub. Nothing in the workflows
can do it: `GITHUB_TOKEN` can push a package but cannot change its visibility.

Artifact Hub cannot index a private chart either, so this step comes before
registration rather than after.

## Artifact Hub

The chart is listed on [Artifact Hub](https://artifacthub.io/packages/search?repo=prometheus-universal-exporter),
which indexes the OCI repository rather than the Git repository. `Chart.yaml`
carries what it shows — the maintainer, home, sources, keywords and
`artifacthub.io/*` annotations — and `.artifacthub-repo.yml` at the repository
root is the ownership file it reads, with the repository ID Artifact Hub
generated at registration.

Nothing needs doing per release: Artifact Hub polls the OCI repository and
picks a new chart version up on its next index run.
After a release, check that the new version appears.

The registration was manual, done once, and is recorded here in case it has to
be redone — for example after the OCI repository moves:

1. Publish at least one chart version, and make the GHCR package public as
   above; Artifact Hub cannot index a private chart.
2. Sign in at [artifacthub.io](https://artifacthub.io).
3. Open the control panel and add a repository.
4. Choose the **Helm charts** kind and the **OCI** flavour.
5. Give it the URL `oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`.
6. Copy the repository ID Artifact Hub generates.
7. Put it in `.artifacthub-repo.yml` as `repositoryID`, then commit and push.

Do not change `repositoryID` otherwise: Artifact Hub would stop verifying
ownership and stop indexing new versions. A test in the Go suite fails if the
file stops parsing, loses its owners, or its `repositoryID` is not a UUID.
