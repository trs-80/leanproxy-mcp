# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

LeanProxy-MCP is a Go CLI (`leanproxy-mcp`) that sits between an AI coding agent and its MCP servers, aggregating them behind one endpoint and cutting token usage (tool-manifest stubbing, response truncation, caching). Module path: `github.com/mmornati/leanproxy-mcp` (this repo is the `trs-80` fork; `main` is the working branch).

## Commands

- `make build-local` — build for the current platform into `dist/`
- `make test` — unit tests; `make test-all` adds lint + E2E
- `make lint` — golangci-lint (CI-pinned version installed via `lint-install`)
- `make bench` — token-economy + NFR benchmarks into `bench-results/`; compare runs with `make bench-compare FILES='old.txt new.txt'`
- `make help` — full target list

Run the full `make test` and `make lint` before claiming work complete or opening a PR.

## Layout

- `main.go`, `cmd/` — CLI entrypoints (cobra commands: `serve`, `add`, `cache`, `report`, `savings`, …), one file per command with its tests alongside
- `pkg/mcp/` — MCP protocol core (handlers are split into per-concern files)
- `pkg/` — supporting packages (bouncer, cache, connpool, gateway, registry, reporter, router, …)
- `internal/cachefile` — atomic cache-file persistence; `internal/netguard` — network egress guard
- `tests/` — `bench`, `e2e`, `integration`, `manual`, `security` suites
- `docs/` + `mkdocs.yml` — user documentation site

## Hard constraints

- **No external network egress from inference paths.** `internal/netguard` confines inference to loopback; the OpenAI embedder was deliberately removed. Do not add code that calls external APIs from the proxy runtime.
- **Never delete or rename `dist/leanproxy-mcp`.** A live agent configuration launches that binary directly. Rebuild in place with `make build-local`.
- **Measure tokens, not nanoseconds.** The product metric is tokens/turn and $/task. Report benchmark results in those terms (`make bench`); ns/op numbers are not meaningful outcomes here.
- Cache writes must stay atomic (temp file + rename) and respect the existing key-sanitization scheme in `internal/cachefile`.

## Conventions

- Go 1.25+. `make fmt` and `make vet` before committing; CI runs the pinned golangci-lint and gosec.
- Tests live next to the code they cover; benchmarks that guard token economy live in `tests/bench` and must not regress.
- Release tooling is GoReleaser (`.goreleaser.yml`); macOS notarization is intentionally disabled — don't re-enable it without Apple signing secrets in place.
