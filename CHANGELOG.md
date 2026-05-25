# Changelog

All notable changes to **amp-proxy-neo** are documented in this file.

## [Unreleased]

### Wave 4 — Local LLM, Autostart, Sync (K/L/M)

#### K · Local LLM adapter
- New `pkg/provider/custom_openai.go` driver targeting OpenAI-compatible local backends (Ollama / LM Studio / vLLM).
- New `pkg/provider/tools_compat.go` schema-fallback layer that downgrades `tools`/`function_call` for models that lack native tool support.
- New admin endpoint `POST /api/llm-probe` reporting `available_models`, `supports_tools`, `supports_streaming`, `latency_ms`, `endpoint_kind`.
- Example configs under `cmd/amp-proxy-neo/examples/{ollama,lmstudio,vllm}.yaml`.

#### L · Autostart, auto-update, tray UX
- New `scripts/install-neo.sh` / `scripts/uninstall-neo.sh` installing a LaunchAgent (`com.fan.amp-proxy.neo`) from `deploy/com.fan.amp-proxy.neo.plist.tmpl`.
- New `internal/neo/updater/` package — channel-aware (`stable` / `beta` / `dev`) GitHub-releases checker.
- New admin endpoint `GET /api/update/check`; tray now exposes update + restart state.
- Tray refactored into a small state machine (idle → checking → update-available → restarting).

#### M · Thread export/import, iCloud sync, backup
- New CLI subcommands on the main binary:
  - `export <thread-id> [-o file] [-format json|ndjson]`
  - `export-all [-o file]`
  - `import <file> [-format auto|json|ndjson|tar.gz]`
  - `list-threads [-limit N] [-format json|table]`
  - `delete-thread <id>`
  - `db-info`
  - `backup [-o dir]`
  - `restore <backup.tar.gz> [-yes]`
- New `internal/neo/threadstore/export.go` (versioned manifest, tar.gz roundtrip).
- New `internal/neo/threadstore/icloud.go` — optional cross-device sync of JSON snapshots (SQLite stays local); conflict log records `local_v` / `cloud_v` decisions.

### Wave 3 — Neo binary wiring (G/H/I/J)
- Full neo binary in `cmd/amp-proxy-neo/`: chat WS, embedded React UI, thread admin endpoints.
- `internal/neo/selfserve/`: JWT signing, `/metadata`, `/actors/*`, `/auth/sign-in`, bootstrap, user-id derivation.
- `internal/neo/rivet/standalone.go` self-serve forwarder (no ampcode.com).
- Per-mode model config + `ResolveByRef` + admin UI for mode editing.
- `GET /api/modes` now wired into the admin mux (fixes the lingering 404 from end of Wave 3).

### Wave 1 / Wave 2 — Foundation
- Forked from `amp-proxy@6d2b44d` to module `github.com/fran0220/amp-proxy-neo`.
- Code split into `pkg/` (shared, no upward imports) and `internal/neo/` (neo-only subpackages).
- `internal/neo/threadstore/` SQLite store (`modernc.org/sqlite`, no CGO).
- `internal/neo/rivet/` Rivet WS gateway; capture script + `RIVET_BOOTSTRAP.md`.
- `cmd/amp-proxy-neo/` entrypoint and `scripts/build-neo.sh` producing `AmpProxyNeo.app`.
- Ports: proxy `9319`, admin `9320`. Config / data root: `~/.amp-proxy-neo/`.
- Default mode is **self-serve** — no ampcode.com dependency.
