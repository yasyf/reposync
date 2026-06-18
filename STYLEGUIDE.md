# reposync Style Guide

The concrete style rules for this repository.

## Core Principles

1. **Fail fast, fail loud.** No defensive coding: no fallbacks, shims, or
   backwards-compat layers, and no guards against impossible states. No sentinel
   values, no silent defaults. If unused, delete it. Crash on the unexpected.
2. **Make invalid states unrepresentable.** Branded/newtype primitives, immutable
   data structures, required fields over optionals.
3. **Minimal changes.** Stay within scope. Make the test pass, then stop. Improve
   only the code you touch.
4. **Match surrounding code.** Follow this guide first, then the file you're in,
   then the module. If surrounding code violates this guide, fix it.

## Go Rules

**Formatting is not a style choice.** `gofmt`/`goimports` own whitespace, import
grouping, and alignment. Never hand-format; never review a diff for it.

**Naming.** Short, lower-case package names with no underscores (`config`, not
`config_loader`). Exported identifiers carry doc comments that start with the
identifier; unexported helpers stay unexported. Don't stutter — `config.Load`, not
`config.LoadConfig`.

**Errors are values, returned not thrown.** Return `error` as the last value and
wrap with context using `%w` so callers can `errors.Is`/`errors.As`. Never panic
for an expected failure (a missing config, a non-git directory). `panic` is for
truly impossible states only.

```go
// Good
raw, err := os.ReadFile(path)
if err != nil {
    return fmt.Errorf("read config: %w", err)
}

// Bad — discards the cause and the call site
raw, err := os.ReadFile(path)
if err != nil {
    return errors.New("could not read config")
}
```

**Accept interfaces, return concrete types.** Keep interfaces small and define them
where they're consumed, not where they're implemented.

**Context first.** Functions that shell out or do I/O take `ctx context.Context` as
their first parameter and honor it (`exec.CommandContext`, not `exec.Command`).

```go
// Good
func git(ctx context.Context, dir string, args ...string) (string, error)

// Bad — uncancellable
func git(dir string, args ...string) (string, error)
```

**No naked returns** in anything longer than a few lines. **No `else` after a
`return`** — prefer early returns and guard clauses over nested blocks.

## Error Handling

Handle each error where it happens; only the call that can fail belongs in the
`if err != nil` block. Wrap with `fmt.Errorf("...: %w", err)` to add context, and
reserve named sentinel errors (`var ErrFoo = errors.New(...)`) for conditions a
caller branches on. Validate required configuration at load time so a missing or
malformed key fails before any work starts, not midway through a sync.

## Code Organization

Order each file: package clause, imports, constants, then types, then their
methods, then free functions. Constants sit immediately after the imports. Control
visibility with capitalization — exported identifiers are part of the package's
contract, so keep the unexported surface as large as it can be.

## Comments & Docstrings

Code documents itself through names, types, and organization. No comments except
TODOs, non-obvious workarounds, or disabled code. Document the public API only;
a doc comment that restates the signature is clutter to delete.

## Testing

Write strict assertions against specific expected values; a test that can't fail
uncovers nothing. Use table-driven tests with `t.Run` subtests for repeated bodies,
giving each case a descriptive name and its own expected values. Prefer exercising
the real boundary over a mock when it's cheap and deterministic: the `internal/sync`
tests run actual `git` against bare repos in `t.TempDir()` rather than faking the
git CLI, which catches argument and behavior mismatches a mock would hide.
