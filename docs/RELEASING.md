# Releases

The exporter and the Helm chart are released independently, each from its own
tag and workflow. A chart fix does not require an exporter release, and an
exporter release does not republish the chart.

## Exporter

An `exporter/prometheus-universal-exporter-vMAJOR.MINOR.PATCH` tag runs
`release.yml`, which publishes the container image to GHCR, builds the
cross-platform archives, and creates a GitHub Release containing them:

```sh
git tag -a exporter/prometheus-universal-exporter-v1.0.0 -m "Exporter v1.0.0"
git push origin exporter/prometheus-universal-exporter-v1.0.0
```

The image is published as `ghcr.io/eenchev/prometheus-universal-exporter:1.0.0`,
also tagged `1.0` when it is the newest release of 1.0, and `latest` when it
is the newest release of all. A patch to an older line, say 1.0.1 after
1.1.0, is published as `1.0.1` and `1.0`, and leaves `latest` on 1.1.0.

## Helm chart

Chart tags are namespaced under `chart/`. Bump `version` in
`charts/prometheus-universal-exporter/Chart.yaml` first — it is the source of
truth, and `release-chart.yml` refuses to publish a tag that disagrees with it:

```sh
# after setting version: 0.2.0 in Chart.yaml
git tag -a chart/prometheus-universal-exporter-0.2.0 -m "Chart 0.2.0"
git push origin chart/prometheus-universal-exporter-0.2.0
```

The workflow re-runs the chart lint and template checks, packages the chart
exactly as `Chart.yaml` declares it, pushes it to
`oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`, and creates a
GitHub Release with the archive.

`appVersion` in `Chart.yaml` records the exporter release a chart version was
validated against and is maintained by hand. It does not drive deployments:
`image.tag` defaults to `latest`, so pin it in your values if you want a
deployment tied to a specific exporter version.

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
