# Contributing to AHV Remote Console

This guide explains how to set up the project, make a change and open a pull request.
Read the [Code of Conduct](CODE_OF_CONDUCT.md) first.

> [!IMPORTANT]
> Report security issues privately as described in [SECURITY.md](SECURITY.md). Do not
> use a public issue or pull request.

## Development setup

You need Go 1.26, Node 24 (see `.nvmrc`) and pnpm 11 (see `packageManager` in
`package.json`). Platform notes are in [Building from source](docs/development/build.md).

```bash
git clone https://github.com/MarcoCarvin/ahv-remote-console.git
cd ahv-remote-console
make install
make dev
```

`make install` installs the pnpm and Go dependencies. `make dev` runs the desktop app
and the runtime in development mode.

Run the checks before you open a pull request:

```bash
make test
make lint
make typecheck
```

| Target | What it runs |
| --- | --- |
| `make test` | Go tests with `-race -cover`, and the TypeScript tests |
| `make lint` | Go linters for the runtime and plugins, and ESLint |
| `make typecheck` | `tsc --noEmit` |

## Workflow

1. Open an issue first for anything larger than a small fix, so you can agree on the
   approach before you write code.
2. Create a branch from `main`. Use a prefix for the change type, for example
   `feat/<scope>`, `fix/<scope>`, `docs/<scope>` or `sec/<scope>`.
3. Keep each pull request to one change. Avoid unrelated refactors.
4. Open a pull request against `main` and fill in the template. CI must pass.
5. A maintainer reviews and merges. `main` stays releasable.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```text
feat(clipboard): add sanitisation for the host-to-guest direction
fix(vdagent): correct the SPICE message type constants
sec(proxy): limit decompressed size of zlib payloads
```

Describe the change on its own terms. Do not add tool or AI attribution lines to
commits, pull requests or code comments.

## Code style

| Area | Rules |
| --- | --- |
| TypeScript | The ESLint configuration applies. Strict types, no `any`, type-only imports, no floating promises. |
| TypeScript formatting | Prettier with 2 spaces, 100-column lines and trailing commas. |
| Go | `gofmt`, `go vet` and `staticcheck` must pass. |
| CSS | Use the design tokens in `packages/ui/src/tokens.css`. Avoid raw hex colours outside the theme files and SVG assets. |
| Comments | Explain why, not what. No internal hostnames, ticket links or personal data. |

## Testing

- New code paths need tests. Bug fixes need a regression test where practical.
- Go: `go test -race ./...` or `make test-go`.
- TypeScript: `vitest` or `make test-ts`.
- Check changes to packaging or signing with the matching `make package*` target.

Run a single test:

```bash
go test -race -run TestName ./apps/runtime/internal/proxy/...
pnpm --filter @ahv-remote-console/desktop test <file-or-pattern>
```

Add `-t "<name>"` to the pnpm command to run one case.

> [!TIP]
> Type-aware ESLint reads the `packages/*` declarations. Build them first with
> `pnpm -r --filter './packages/**' build`, or you get many false `no-unsafe-*`
> errors.

See [Testing](docs/development/testing.md) for what each CI gate covers.

## Documentation

Documentation lives in [`docs/`](docs/README.md). If your change alters behaviour,
configuration or the architecture, update the matching document in the same pull
request. Report documentation that no longer matches the code.

Write documentation in plain English:

- Say what the code does. Use short sentences, the active voice and the present tense.
- Do not use filler or hype words such as `seamless`, `robust`, `leverage` or
  `powerful`, and do not use emoji.
- Link source files by path. Do not cite line numbers, because they go stale.
- Use the terms in the [glossary](docs/reference/glossary.md).

CI runs `scripts/check-plain-words.sh`, which fails on the most common filler words.
Run it locally before you open a pull request:

```bash
scripts/check-plain-words.sh
```

## Dependencies and licences

- Dependabot opens update pull requests for Go, npm and GitHub Actions.
- After you add or upgrade a dependency, run `pnpm notices:generate` and commit
  `THIRD_PARTY_NOTICES.md`. The `build` workflow fails if the file is out of date.
- Contributions are accepted under the [Apache License 2.0](LICENSE).

## Security-sensitive changes

Changes to authentication, IPC signing, cryptography, the clipboard path or network
boundaries need extra review. Say so in the pull request description and request
review from a maintainer who knows that area.

## Related documentation

- [Architecture overview](docs/architecture/overview.md)
- [Building from source](docs/development/build.md)
- [Testing](docs/development/testing.md)
- [Protocol reference](docs/reference/protocol.md)
- [Governance](GOVERNANCE.md)
