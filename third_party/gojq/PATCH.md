# gojq, with one change

This directory is [gojq](https://github.com/itchyny/gojq) v0.12.20, the
version `go.mod` requires, which `go.mod` replaces the module with. It is
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
- `env.go` and `execute.go` have the lines below: after a function gave a
  value, a text or an array larger than `bytes` is told by a call of
  `exceeded`, unless it is a value the function was given, or a part or an
  item of one, which it did not make. Nothing else of a program's run
  changes, and a context without the limit costs one comparison a call.
- `builtin.go` is as `gofmt` writes it, which the repository's formatting
  check asks of every Go file; upstream generates it unformatted.

```diff
--- a/env.go
+++ b/env.go
@@ -17,6 +17,7 @@
 	label     int
 	args      [32]any // len(env.args) > maxarity
 	ctx       context.Context
+	most      *maxValue // prometheus-universal-exporter: see maxvalue.go
 }
 
 func newEnv(ctx context.Context) *env {
@@ -25,6 +26,7 @@
 		paths:  newStack[pathValue](),
 		scopes: newStack[scope](),
 		ctx:    ctx,
+		most:   maxValueOf(ctx),
 	}
 }
 
--- a/execute.go
+++ b/execute.go
@@ -189,6 +189,9 @@
 					err = e
 					break loop
 				}
+				if env.most != nil {
+					env.most.made(w, x, args, v[2].(string)) // prometheus-universal-exporter: see maxvalue.go
+				}
 				env.push(w)
 				if !env.paths.empty() && env.expdepth == 0 {
 					switch v[2].(string) {
```

The exporter uses it in `internal/expr/memory.go`, and the tests named
`TestAValueLargerThanTheMemoryLimit…` there fail without it.

`govulncheck` and image scanners match a module by its version, and this
copy has none: an advisory for gojq has to be looked for by hand. A module
with a `replace` in its `go.mod` is also not one `go install
<module>@<version>` builds; the exporter is built from a checkout, as the
image and the release are.

## Moving to a newer gojq

Dependabot leaves this module alone (`.github/dependabot.yml`), since the
version in `go.mod` only names what is copied here.

1. Set the new version in `go.mod`'s `require` line, and take the
   `replace` line out for the moment: `go mod download
   github.com/itchyny/gojq@<version>`.
2. Copy the module's `*.go` files, without `*_test.go`, and its `LICENSE`
   over this directory, keeping `maxvalue.go`, `PATCH.md` and `go.mod`
   (whose `require` is what the new sources import outside the standard
   library).
3. Apply the diff above again, run `gofmt -w builtin.go`, and put the
   `replace` line back: `go mod tidy`.
4. Run the tests of `internal/expr` and `internal/transform`.
