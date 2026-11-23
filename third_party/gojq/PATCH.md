# gojq, with one change

This directory is [gojq](https://github.com/itchyny/gojq) at the release
`go.mod` requires and `UPSTREAM.sha256` names, which `go.mod` replaces the
module with. It is
the library's Go sources alone, under its own licence (`LICENSE`): no tests,
no command-line tool, no generator.

## What is changed, and why

A jq program that doubles a value at every step, `. + .` on a text, holds
gigabytes within a second, and one such step cannot be interrupted. The
exporter bounds what a program may hold with `limits.max_expression_memory`
by watching the heap, which finds such a program only after it has passed
the limit many times over. gojq's options give no way to see the value an
operator made, so the exporter is told from inside the interpreter:

- `maxvalue.go` is new: `WithMaxValueBytes(ctx, bytes, exceeded)`, and what
  it keeps in the context.
- `env.go` and `execute.go` have three lines more: after a function gave a
  value, a text, an array or an object larger than `bytes` is told by a call of
  `exceeded`, unless it is a value the function was given, or a part or an
  item of one, which it did not make. Nothing else of a program's run
  changes, and a context without the limit costs one comparison a call.
- `builtin.go` is as `gofmt` writes it, which the repository's formatting
  check asks of every Go file; upstream generates it unformatted.

The lines are `gojq.patch`, which is what is applied to a release.

The exporter uses it in `internal/expr/memory.go`, and the tests named
`TestAValueLargerThanTheMemoryLimit…` there fail without it.

`govulncheck` and image scanners match a module by its version, and this
copy has none: an advisory for gojq has to be looked for by hand. A module
with a `replace` in its `go.mod` is also not one `go install
<module>@<version>` builds; the exporter is built from a checkout, as the
image and the release are.

## Keeping the copy what it says it is

`UPSTREAM.sha256` lists the checksum of every file as gojq released it. A
test of the repository (`TestTheCopyOfGojqIsUpstreamsWithThePatchAndNothingElse`)
takes `gojq.patch` out of the copy and holds each file to its checksum, so
a change made here by hand fails it: a change to gojq goes into the patch
and this file.

## Moving to a newer gojq

Dependabot leaves this module alone (`.github/dependabot.yml`), since the
version in `go.mod` only names what is copied here. A workflow
(`.github/workflows/gojq-release.yml`) opens an issue when gojq has a newer
release than the copy.

```sh
sh tools/gojq-copy.sh v0.12.21
```

downloads the release, replaces the sources here with its `*.go` files
(without tests) and its `LICENSE`, writes their checksums, applies
`gojq.patch` and writes it again against the release, runs `gofmt` on
`builtin.go`, and has `go.mod` require the release. It stops where the
patch no longer applies, which is then made again by hand. After it:

1. Hold `go.mod` here to the release's: its `require` is what the sources
   import outside the standard library.
2. Read the release's notes for what changed in how a function gives its
   value, which is where the patch reads it.
3. Run the tests of `internal/expr`, `internal/transform` and
   `test/repository`.
