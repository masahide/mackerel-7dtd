# Repository Guidelines

## Project Structure & Module Organization
- `main.go`: Posts 7 Days to Die (7dtd) player metrics to Mackerel.
- `pkg/telnet/`: Telnet client and parsers for 7dtd server output (players, time).
- `playerCountBot/`: Discord bot that shows server time and player count via presence.
- Root: `go.mod`, `go.sum`, `README.md`.

## Build, Test, and Development Commands
- Build main app: `go build -o mackerel-7dtd .` — produces the CLI binary.
- Run main app: `go run .` — useful for local debugging.
- Build Discord bot: `go build -o playerCountBot/playerCountBot ./playerCountBot`.
- Run Discord bot: `go run ./playerCountBot`.
- Format: `go fmt ./...` — standardize formatting.
- Vet: `go vet ./...` — static checks for common issues.

## Coding Style & Naming Conventions
- Language: Go 1.24. Follow idiomatic Go; run `go fmt` before pushing.
- Indentation: tabs as emitted by `gofmt`.
- Packages/files: short, lowercase names (e.g., `telnet`, `time.go`).
- Exported identifiers: `UpperCamelCase` with doc comments; unexported use `lowerCamelCase`.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`) and prefer early returns.

## Testing Guidelines
- Framework: Go `testing` package.
- Location & names: `_test.go` files in the same package; tests named `TestXxx`.
- Run all tests: `go test ./...` (add focused tests under `pkg/telnet` for parsers).
- Table-driven tests for parsing functions are encouraged; keep tests deterministic.

## Commit & Pull Request Guidelines
- Commits observed are short (e.g., `fix:`). Use imperative mood, keep scope small.
- Prefer Conventional Commits where helpful: `feat:`, `fix:`, `refactor:`, `test:`.
- PRs must include: what/why summary, linked issues, run instructions, and env vars impacted.
- CI hygiene: ensure `go fmt`, `go vet`, and `go build` succeed before requesting review.

## Security & Configuration Tips
- Configuration (env): `MACKEREL_HOST_ID`, `MACKEREL_API_KEY`, `SERVERADDR` (default `localhost:8081`), `TELNETPASS`, and for the bot `DISCORD_TOKEN`, `DISCORD_SERVER_ID`.
- Example (local): `MACKEREL_HOST_ID=... MACKEREL_API_KEY=... TELNETPASS=... go run .`
- Do not commit secrets; use environment variables or your secrets manager.
- Deployment: run the built binary via cron/systemd; ensure network access to the 7dtd telnet port.
