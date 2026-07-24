# fix-api-spec

Canonicalizes the enums in an OpenAPI spec so that [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen)
generates stable Go constant names.

## The problem

oapi-codegen has to invent a Go identifier for every enum value in a spec. For an
enum that isn't a named schema, it derives that identifier from context, and it
only prefixes the constant with its type name when it has to — when two enums in
the spec would otherwise produce the same constant name.

That "only when it has to" is the problem: whether a constant is named `Daily` or
`IntervalDaily` depends on what *other* enums exist elsewhere in the spec. Adding
an unrelated enum that happens to share a value silently renames constants on an
enum nobody touched, and every downstream package stops compiling.

Inline enums on parameters make it worse. A parameter referenced by `$ref` from
several endpoints generates one type *per endpoint*:

```
GetAnalyticsPerformanceParamsSortDirection
GetTransactionsParamsSortDirection
...
```

One logical enum, N Go types, all named after the operations that happen to use
it — so renaming a path or an `operationId` renames the types too.

Neither failure is caught by the spec being valid, or by the generator; you find
out when the generated code changes underneath you.

## The fix

The tool reads the **existing** `server.gen.go` to learn the type and constant
names currently in use, then rewrites the spec to pin exactly those names. The
generated code is the source of truth, so today's names are frozen rather than
recomputed.

It enforces four requirements:

1. **Every enum is a named schema under `components/schemas`.**
   Inline enums — on properties, path and query parameters, request bodies, and
   array items — are extracted into a standalone schema, and the original site is
   replaced with `$ref: '#/components/schemas/<Name>'`.

2. **Every enum schema carries `x-enum-varnames`.**
   This is the extension oapi-codegen honors for explicit constant names. Entries
   are written in the same order as that schema's `enum` values, since the two
   lists are matched positionally.

3. **Constant names are fully qualified.**
   A constant must be prefixed with its type name — `IntervalDaily`, not `Daily`.
   Any short name found in `server.gen.go` is rewritten to
   `<TypeName><PascalCase(value)>` and reported as a rename.

4. **Named parameters don't carry inline enums.**
   An enum on a `components/parameters/*` entry is hoisted into
   `components/schemas` and the parameter's `schema` becomes a `$ref`, collapsing
   the per-endpoint types above into one shared type.

New schemas are inserted in alphabetical order as `type: string` with a flow-style
`enum` list. The tool is idempotent — running it again produces the same file.

If the spec contains an enum value with no matching constant in `server.gen.go`,
it warns and falls back to using the raw value as the varname.

## Usage

```
fix-api-spec -gen <server.gen.go> -spec <api-spec.yaml> [-out <output.yaml>] [-check]
```

- `-out` defaults to `-spec`, rewriting the spec in place.
- `-check` writes nothing and exits non-zero if the spec would change or if any
  constants need renaming. Intended as a build gate ahead of `go generate`, so a
  drifted spec fails the build instead of silently regenerating different names.

The Makefile takes the file locations from the environment, since they live in the
consuming repo rather than here:

```sh
export API_GEN=/path/to/server.gen.go     # generated code to read names from
export API_SPEC=/path/to/api-spec.yaml    # spec to canonicalize
export API_SPEC_OUT=...                   # optional; defaults to <spec>-fixed.yaml,
                                          # set equal to API_SPEC to fix in place

make run      # fix the spec
make check    # verify it's already canonical, write nothing
```

`make` on its own just builds; `make clean` removes the binary and the fixed spec.

## Workflow

Because the generated code is the source of truth, the order matters:

1. Edit the spec.
2. Regenerate with oapi-codegen.
3. Run `make run` to pin any new enums, then regenerate once more.

From then on `make check` will pass, and the constant names stay put no matter
what else is added to the spec.
