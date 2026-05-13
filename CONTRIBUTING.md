# Contributing to resleeve

Resleeve is pre-release. Design discussion happens in [GitHub Discussions](https://github.com/mattkorwel/resleeve/discussions); bugs and features in [Issues](https://github.com/mattkorwel/resleeve/issues); code via Pull Requests.

## Before contributing code

1. Read the [design docs](docs/design/) — they cover the full architectural intent across three rounds.
2. For non-trivial changes, open a Discussion or Issue first so we can align on direction.
3. Follow [Conventional Commits](https://www.conventionalcommits.org/) for commit messages.

## Code style

- `go vet ./... && go test ./...` must pass before merge.
- Linting via `golangci-lint`.
- No backend-specific SQL in `internal/storage/sql/` — see the [storage backends doc](docs/design/round-2/11-storage-backends.md) for portability rules.

## License

By contributing, you agree your contributions will be licensed under [Apache-2.0](LICENSE).
