# amp-proxy-neo

Standalone local backend for [Amp](https://ampcode.com) **Neo** mode.

Forked from [amp-proxy](https://github.com/fran0220/amp-proxy) (legacy provider proxy) to develop the Neo experience independently:

- **Rivet WebSocket gateway** intercept + local inference orchestration
- **Local SQLite thread store** (no ampcode.com account required, planned)
- **Self-serve mode** (no upstream dial, planned Wave 3)
- **Browser chat UI** direct (no amp CLI subprocess needed)
- **Per-mode model config** (custom provider/model/auth per agent mode)
- Shares local OAuth credentials with `amp-proxy` (Claude Keychain, Codex auth.json, Gemini oauth_creds.json) — both apps can coexist

## Ports

| App | Proxy | Admin/Web UI |
|---|---|---|
| `amp-proxy` (legacy) | `:9317` | `:9318` |
| `amp-proxy-neo` (this) | `:9319` | `:9320` |

## Config

- `~/.amp-proxy-neo/config.yaml`
- `~/.amp-proxy-neo/threads.db` (local thread store)
- `~/.amp-proxy-neo/amp-proxy-neo.db` (request logs)
- `~/.amp-proxy-neo/proxy.log`

## Build

```bash
go build ./cmd/amp-proxy-neo
./scripts/build-neo.sh   # → AmpProxyNeo.app for macOS
```

## Status

| Module | Status |
|---|---|
| `pkg/{config,auth,token,provider,logger,...}` | ✅ Shared base (from amp-proxy Wave 1) |
| `internal/neo/threadstore` | ✅ Local SQLite store + tests |
| `internal/neo/rivet` | ✅ Rivet WS gateway + local inference (carried over) |
| `internal/neo/remoteagent` | ✅ CF Worker remote-drive bridge |
| `internal/neo/selfserve` | 🟡 Bootstrap spec + fixture, synthesis pending Wave 3 |
| `cmd/amp-proxy-neo` | 🟡 Skeleton, full wiring pending Wave 2 PR4 |
| Per-mode model config | ⏳ Wave 3 |
| Browser chat WS direct | ⏳ Wave 3 |
| Self-serve `/metadata` `/actors` `/auth` stubs | ⏳ Wave 3 |
| Self-serve JWT sign | ⏳ Wave 3 |
| Local LLM (Ollama/LM Studio) adapter | ⏳ Wave 4 |
| LaunchAgent / autostart | ⏳ Wave 4 |

See [`AGENTS.md`](./AGENTS.md) and [`docs/RIVET_BOOTSTRAP.md`](./docs/RIVET_BOOTSTRAP.md).
