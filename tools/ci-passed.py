"""Wait until CI has passed on a commit, and fail when it has not.

The release workflows run on a tag push whatever CI said about the commit the
tag names, so each runs this first and publishes nothing unless it succeeds.
CI has passed on a commit when the newest finished run of the CI workflow
(ci.yml) that a push of that very commit started, and that ran the suite,
concluded `success`. A run that failed, or ended any other way than in
success, counts whatever it ran. A run that succeeded counts only when its
step SUITE_STEP did: a push whose changed paths ran none of the Go steps
passes with every step skipped, and says nothing about the commit, so a
failure before it is not taken back by it. A run a pull request started is
not counted: it tested the pull request merged into its base, not the
commit.

A tag is often pushed right after its commit, and the tag push starts a CI
run of its own, so a commit whose counted runs are all still queued or
running, or that has none yet, is waited for, up to --timeout seconds. A
commit whose runs have all finished fails at once when the newest counted
one did not succeed, or when none ran the suite.

Reads the GitHub API at $GITHUB_API_URL (https://api.github.com when unset)
with $GITHUB_TOKEN, which needs the `actions: read` permission:

    python3 tools/ci-passed.py --repository owner/name --sha <commit>
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

WORKFLOW = "ci.yml"

# The step of ci.yml that runs whenever the Go suite does; a run whose step of
# this name concluded success ran the suite. A test keeps the two in step.
SUITE_STEP = "Run the repository's checks"


class Refused(Exception):
    """The API answered with an error that asking again does not change."""


def get(url: str, token: str):
    """Returns the JSON the API answers to url."""
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {token}",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            body = json.load(response)
    except urllib.error.HTTPError as err:
        # A server error or a rate limit can pass; a refusal of the request or
        # of the token does not.
        if err.code >= 500 or err.code == 429:
            raise
        raise Refused(
            f"the GitHub API answered {err.code} to {url}: give the job "
            f"`actions: read` and check the repository name"
        ) from err
    return body


def runs_of(api: str, repository: str, sha: str, token: str) -> list:
    """Returns the CI runs a push of sha started."""
    query = urllib.parse.urlencode({"head_sha": sha, "event": "push", "per_page": "100"})
    body = get(f"{api}/repos/{repository}/actions/workflows/{WORKFLOW}/runs?{query}", token)
    # The query asks for these runs alone; they are picked out again in case
    # the API ever lets one through.
    return [
        run
        for run in body.get("workflow_runs") or []
        if run.get("head_sha") == sha and run.get("event") == "push"
    ]


def ran_suite(api: str, repository: str, token: str, run: dict) -> bool:
    """Reports whether the step SUITE_STEP of a run concluded success."""
    query = urllib.parse.urlencode({"filter": "latest", "per_page": "100"})
    body = get(f"{api}/repos/{repository}/actions/runs/{run.get('id')}/jobs?{query}", token)
    return any(
        step.get("name") == SUITE_STEP and step.get("conclusion") == "success"
        for job in body.get("jobs") or []
        for step in job.get("steps") or []
    )


def verdict(runs: list, ran) -> tuple:
    """Returns ("passed" | "failed" | "waiting", what to report); ran(run)
    reports whether a run that succeeded ran the suite."""
    finished = [run for run in runs if run.get("status") == "completed"]
    pending = [run for run in runs if run.get("status") != "completed"]
    # The newest counted verdict decides: a run started again after a failure
    # and passing is CI passing, and one failing after a pass is not. A run
    # that succeeded without running the suite is passed over, so the
    # failure before it still stands.
    finished.sort(key=lambda run: (run.get("updated_at") or "", run.get("id") or 0), reverse=True)
    for run in finished:
        described = f"{WORKFLOW} run {run.get('html_url') or run.get('id')}"
        if run.get("conclusion") != "success":
            if pending:
                break
            return "failed", f"{described} concluded {run.get('conclusion')}"
        if ran(run):
            return "passed", f"{described} concluded success"
    if pending:
        states = ", ".join(sorted({run.get("status") or "unknown" for run in pending}))
        return "waiting", f"{len(pending)} {WORKFLOW} run(s) not finished ({states})"
    if finished:
        return "failed", (
            f"no {WORKFLOW} run of a push of this commit ran the suite: each skipped "
            f"the step {SUITE_STEP!r}"
        )
    return "waiting", f"no {WORKFLOW} run of a push of this commit yet"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--repository", required=True, help="owner/name")
    parser.add_argument("--sha", required=True, help="the commit the tag names")
    parser.add_argument("--timeout", type=float, default=3600, help="seconds to wait for CI to finish")
    parser.add_argument("--interval", type=float, default=30, help="seconds between two looks")
    args = parser.parse_args()
    token = os.environ.get("GITHUB_TOKEN", "")
    if not token:
        print("GITHUB_TOKEN is not set: pass the job's token in the step's env.", file=sys.stderr)
        return 1
    api = (os.environ.get("GITHUB_API_URL") or "https://api.github.com").rstrip("/")
    advice = (
        f"CI has not passed on {args.sha}: run CI or wait for it, then push the tag "
        f"again or re-run this release"
    )
    deadline = time.monotonic() + args.timeout
    while True:
        try:
            state, detail = verdict(
                runs_of(api, args.repository, args.sha, token),
                lambda run: ran_suite(api, args.repository, token, run),
            )
        except Refused as err:
            print(f"{advice} ({err}).", file=sys.stderr)
            return 1
        except (urllib.error.URLError, OSError, ValueError) as err:
            state, detail = "waiting", f"the GitHub API could not be read ({err})"
        if state == "passed":
            print(f"CI has passed on {args.sha}: {detail}.")
            return 0
        if state == "failed":
            print(f"{advice} ({detail}).", file=sys.stderr)
            return 1
        left = deadline - time.monotonic()
        if left <= 0:
            print(f"{advice} (still waiting after {args.timeout:g} seconds: {detail}).", file=sys.stderr)
            return 1
        print(f"waiting for CI on {args.sha}: {detail}.", flush=True)
        time.sleep(min(args.interval, left))


if __name__ == "__main__":
    sys.exit(main())
