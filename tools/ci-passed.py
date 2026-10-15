"""Wait until CI has passed on a commit, and fail when it has not.

The release workflows run on a tag push whatever CI said about the commit the
tag names, so each runs this first and publishes nothing unless it succeeds.
CI has passed on a commit when the newest finished run of the CI workflow
(ci.yml) that a push of that very commit started concluded `success`; the
steps that run concluded that run, so a run whose changed paths skipped the
Go suite counts as it concluded. A run a pull request started is not counted:
it tested the pull request merged into its base, not the commit.

A tag is often pushed right after its commit, and the tag push starts a CI
run of its own, so a commit whose runs are all still queued or running, or
that has no run yet, is waited for, up to --timeout seconds. A commit whose
runs have all finished, the newest of them not in success, fails at once.

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


class Refused(Exception):
    """The API answered with an error that asking again does not change."""


def runs_of(api: str, repository: str, sha: str, token: str) -> list:
    """Returns the CI runs a push of sha started."""
    query = urllib.parse.urlencode({"head_sha": sha, "event": "push", "per_page": "100"})
    url = f"{api}/repos/{repository}/actions/workflows/{WORKFLOW}/runs?{query}"
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
    # The query asks for these runs alone; they are picked out again in case
    # the API ever lets one through.
    return [
        run
        for run in body.get("workflow_runs") or []
        if run.get("head_sha") == sha and run.get("event") == "push"
    ]


def verdict(runs: list) -> tuple:
    """Returns ("passed" | "failed" | "waiting", what to report)."""
    finished = [run for run in runs if run.get("status") == "completed"]
    pending = [run for run in runs if run.get("status") != "completed"]
    if finished:
        # The newest verdict decides: a run started again after a failure and
        # passing is CI passing, and one failing after a pass is not.
        newest = max(finished, key=lambda run: (run.get("updated_at") or "", run.get("id") or 0))
        described = f"{WORKFLOW} run {newest.get('html_url') or newest.get('id')}"
        if newest.get("conclusion") == "success":
            return "passed", f"{described} concluded success"
        if not pending:
            return "failed", f"{described} concluded {newest.get('conclusion')}"
    if pending:
        states = ", ".join(sorted({run.get("status") or "unknown" for run in pending}))
        return "waiting", f"{len(pending)} {WORKFLOW} run(s) not finished ({states})"
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
            state, detail = verdict(runs_of(api, args.repository, args.sha, token))
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
