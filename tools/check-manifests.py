"""Fail when a rendered chart merges two manifests into one YAML document.

`helm template` prints whatever a template produced; it never checks that each
manifest starts its own document. A template that forgets a `---` therefore
renders output helm is perfectly happy with and Kubernetes rejects. Comparing
the number of manifests rendered with the number of documents the stream parses
into catches exactly that: a missing separator makes two manifests share a
document, so the manifest count exceeds the document count.

Reads the rendered chart on standard input.
"""

import sys

import yaml


def main() -> int:
    rendered = sys.stdin.read()
    # A duplicated mapping key is not an error to PyYAML — it keeps the last
    # value — so the merge has to be detected by counting rather than by
    # parsing strictly.
    documents = [document for document in yaml.safe_load_all(rendered) if document]
    manifests = sum(1 for line in rendered.splitlines() if line.startswith("kind:"))
    if manifests != len(documents):
        print(
            f"{manifests} manifests were rendered but the output parses as "
            f"{len(documents)} YAML documents: a `---` separator is missing.",
            file=sys.stderr,
        )
        return 1
    print(f"{manifests} manifests, each its own YAML document")
    return 0


if __name__ == "__main__":
    sys.exit(main())
