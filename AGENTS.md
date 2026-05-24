# amp-proxy

A local reverse proxy for [Amp](https://ampcode.com) that intercepts LLM API requests and routes them through local CLI credentials or API keys instead of the Amp upstream, with an admin dashboard for monitoring and configuration.

## Architecture

amp-proxy runs **two HTTP servers** on the main thread + a macOS system tray:

| Component | Port | Purpose |
|-----------|------|---------|
| **Proxy server** | `:9317` | Intercepts Amp's `/api/provider/{provider}/...` requests, routes to direct API or upstream |
| **Admin server** | `:9318` | REST API + embedded web dashboard for config, logs, and stats |
| **System tray** | — | macOS menu bar icon showing status, token info, and quick actions |

### Request flow

```
Amp CLI → :9317 → Router
  ├─ WebSocket / non-provider path → forward to ampcode.com upstream
  ├─ POST /api/provider/{provider}/... with model in body/URL
  │   ├─ Route = "amp" → forward to upstream
  │   ├─ Route = "local" → resolve CLI credentials → direct API call
  │   └─ Route = "apikey" → use configured API key → direct API call
  └─ Fallback chain: local → apikey → amp (if credentials unavailable)
```

## File Structure

### Go source files

| File | Description |
|------|-------------|
| `cmd/amp-proxy/main.go` | Entry point — creates token managers, logger, auth resolver, router; starts proxy + admin servers; runs systray on main thread |
| `cmd/amp-proxy/tray.go` | macOS system tray — status icon (green/red), provider summaries, token refresh, dashboard launcher |
| `pkg/config/` | Config struct/defaults, YAML load/save, model route management, multi-key API key CRUD, `AmpModelRoles` |
| `pkg/auth/` | `AuthResolver`, `ProviderAuth`, route fallback, permissions settings |
| `pkg/token/` | Claude profiles/OAuth, Codex token lifecycle, Gemini token lifecycle |
| `pkg/keychain/` | macOS Keychain reader — uses `security` CLI to read Claude Code OAuth credentials |
| `pkg/provider/` | Claude/OpenAI/Codex/Gemini direct provider handlers and API-key validation URL helpers |
| `pkg/identity/` | Injects Claude Code billing header + agent identity into system prompt; renames conflicting tools (`glob` → `file_glob`) |
| `pkg/logger/` | Request logging, SQLite persistence, stats queries, token usage parsers |
| `pkg/retry/` | `Retryer` — automatic retry with backoff on 429/502/503/529, respects `Retry-After` header |
| `pkg/adminbase/` | Admin API handlers, thread proxy API, provider testing/model discovery; static `web/` FS is injected by the entry point |
| `pkg/util/` | Traffic dumps, version/updater, tray icon bitmaps, small shared utilities |
| `router.go` | Legacy HTTP router — extracts provider/model, resolves auth, dispatches to pkg provider handlers or ampcode.com upstream |
| `upstream.go` | `UpstreamProxy` — reverse proxy to ampcode.com for unhandled/amp-routed requests |
| `web_embed.go` | Embeds the legacy admin dashboard static files from `web/` |
| `internal/neo/rivet/` | Neo-only Rivet gateway, local inference, persistence, frame writer/tap, and OpenAI Responses WebSocket code. Used only by `cmd/amp-proxy-neo` (Wave 3+); legacy does not import it. |
| `internal/neo/remoteagent/` | Neo-only outbound remote-agent package. Used only by `cmd/amp-proxy-neo` (Wave 3+); legacy does not import it. |
| `internal/neo/threadstore/` | Neo SQLite-backed thread store and `/api/internal?*` compatible HTTP handler |
| `internal/neo/selfserve/` | Neo bootstrap fixture loader/tests for self-serve Rivet bootstrap work |
| `build-macos.sh` | Builds a macOS `.app` bundle with Info.plist and auto-generated icon |

### Web UI (`web/`)

Embedded into the binary via `go:embed`. Vanilla JS, no build step.

| File | Description |
|------|-------------|
| `index.html` | Shell — topbar, sidebar navigation (5 tabs), content area |
| `style.css` | Dark theme stylesheet |
| `api.js` | API client — `API.get()` / `API.post()` helpers for admin endpoints |
| `app.js` | Router — tab navigation, topbar auto-refresh, init |
| `overview.js` | Overview tab — uptime, request stats, provider status cards, recent logs |
| `providers.js` | Providers tab — API key management (add/remove/test), custom provider CRUD, model discovery |
| `models.js` | Models tab — per-model route toggle (amp/local/apikey), role descriptions, tier compatibility |
| `logs.js` | Logs tab — paginated request log table with provider/route/status filters |
| `stats.js` | Stats tab — daily/hourly charts, per-model breakdown, route distribution, token totals |

## Key Concepts

### Routing: amp / local / apikey

Each model has a **route** that determines how requests are authenticated:

- **`amp`** — Forward to ampcode.com upstream (uses Amp subscription credits)
- **`local`** — Use local CLI credentials (Claude Keychain, Codex file, Gemini file)
- **`apikey`** — Use a configured API key from config

Routes are configured per-model in `config.yaml` and can be changed via the admin dashboard.

### Auth Resolution with Fallback Chain

`AuthResolver.Resolve()` follows this logic:

1. Look up the model's configured route
2. Check tier compatibility (`ModelSupportsTier`) — e.g., `gemini-3-pro-image-preview` is NOT available via `local`
3. Attempt to resolve credentials for that route
4. **Fallback**: if `local` fails → try `apikey` → fall back to `amp`

### Multi-Key Support

Each provider supports multiple API keys via `entries[]` in config. The legacy single `api-key` field is also supported and merged into the entries list with ID `_legacy`. Keys can have per-key `base-url` overrides for custom endpoints.

### Custom Providers

OpenAI-compatible custom providers can be added via the admin dashboard. They get their own entries in `config.yaml` under `custom[]` with name, base URL, API keys, and models.

## Token Lifecycle

### Claude (macOS Keychain)

- **Source**: macOS Keychain service `"Claude Code-credentials"` → `claudeAiOauth` JSON field
- **Load**: On startup via `security find-generic-password` CLI
- **Refresh**: OAuth token refresh to `api.anthropic.com/v1/oauth/token` using client ID `9d1c250a-...`
- **Auto-refresh**: Background loop every 1 minute; refreshes 5 minutes before expiry
- **Fallback**: If refresh fails, reloads from Keychain (user may have re-authenticated)

### OpenAI/Codex (File)

- **Source**: `~/.codex/auth.json` → `tokens.access_token` / `tokens.refresh_token`
- **Refresh**: OAuth token refresh to `auth.openai.com/oauth/token` using client ID `app_BRhCaGoa5MNBp2SRmgiYeMkz`
- **Auto-refresh**: Same pattern — 1-minute loop, 5-minute margin
- **Fallback**: Reloads from file if refresh fails

### Gemini (File + OAuth Refresh)

- **Primary source**: `~/.gemini/oauth_creds.json` (Gemini CLI format) — uses well-known Gemini CLI client credentials
- **Fallback source**: `~/.config/gcloud/application_default_credentials.json` — uses gcloud's own client ID/secret
- **Refresh**: OAuth token refresh to `oauth2.googleapis.com/token` (form-encoded POST)
- **Auto-refresh**: Same pattern; if both refresh and file reload fail, the error cascades

All token managers use `singleflight.Group` to deduplicate concurrent refresh attempts.

## Admin API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/status` | Server status, uptime, token info, request totals |
| GET | `/api/overview` | Dashboard overview — stats, recent logs, provider summaries |
| GET | `/api/config` | Current config (keys masked) |
| POST | `/api/config/model` | Set model route `{provider, model, route}` |
| GET | `/api/stats` | Aggregate request stats by model |
| GET | `/api/stats/daily?days=N` | Daily stats for last N days |
| GET | `/api/stats/hourly?hours=N` | Hourly stats for last N hours |
| GET | `/api/stats/routes` | Stats grouped by route |
| GET | `/api/stats/tokens` | Token totals (input/output/cache) |
| GET | `/api/logs?limit=N&offset=N` | Paginated request logs |
| GET | `/api/logs?provider=X&route=Y&status=N` | Filtered logs |
| GET | `/api/logs/errors?limit=N` | Error logs with request/response bodies |
| POST | `/api/token/refresh` | Force Claude token refresh |
| POST | `/api/provider` | Update provider config (API key, base URL, models) |
| GET | `/api/model-roles` | All model role definitions |
| GET | `/api/model-tiers` | Model tier compatibility |
| GET | `/api/auth/status` | Auth status per provider (local/apikey availability) |
| POST | `/api/auth/route` | Set model auth route |
| GET | `/api/keys?provider=X` | List API keys (masked) for a provider or all |
| POST | `/api/keys/add` | Add API key `{provider, label, api_key, base_url}` |
| POST | `/api/keys/remove` | Remove API key `{provider, id}` |
| POST | `/api/keys/test` | Test API key connectivity |
| POST | `/api/keys/discover` | Discover available models for a provider/key |
| GET/POST/DELETE | `/api/custom-provider` | CRUD custom OpenAI-compatible providers |
| GET/POST | `/api/amp-config` | Get/set Amp upstream URL and API key |
| POST | `/api/provider/add-model` | Add model to provider |
| POST | `/api/provider/delete-model` | Remove model from provider |

## Web UI

The dashboard at `http://localhost:9318` has 5 tabs:

1. **Overview** — Uptime, total requests/errors/tokens, provider cards with auth status, recent request log
2. **Providers** — API key management per provider (add/remove/test keys), custom provider CRUD, model discovery
3. **Models** — Per-model route selector (amp/local/apikey), model role descriptions, tier compatibility indicators
4. **Logs** — Paginated request log table with filters by provider, route, and HTTP status; auto-refresh
5. **Stats** — Daily/hourly request charts, per-model token breakdown, route distribution, cumulative token totals

## Build

### Development binary

```bash
go build -o amp-proxy .
./amp-proxy
```

### macOS .app bundle

```bash
./build-macos.sh
# Creates "AMP Proxy.app" — a menu bar application (LSUIElement=true)
cp -r "AMP Proxy.app" /Applications/
```

The build script compiles an arm64 binary, creates the `.app` bundle structure, generates an icon, and packages everything with an `Info.plist`.

## Config

**Location**: `~/.amp-proxy/config.yaml` (created with defaults on first run)

**Database**: `~/.amp-proxy/amp-proxy.db` (SQLite with WAL mode)

### Config structure

```yaml
listen: ":9317"

amp:
  upstream-url: "https://ampcode.com"
  api-key: "sgamp_user_..."          # Amp subscription key

claude:
  source: keychain                    # "keychain" or "manual"
  api-key: "sk-ant-..."              # Legacy single key
  entries:                            # Multi-key support
    - id: "abc123"
      label: "Production"
      api-key: "sk-ant-..."
      base-url: ""                    # Optional per-key base URL
  models:
    - name: claude-sonnet-4-6
      route: local                    # amp | local | apikey

openai:
  api-key: "sk-..."
  base-url: ""                        # Custom base URL (e.g. Azure)
  entries: [...]
  models:
    - name: gpt-5.4
      route: amp

gemini:
  api-key: "AIza..."
  base-url: ""
  entries: [...]
  models:
    - name: gemini-3.1-pro-preview
      route: amp

custom:                               # Custom OpenAI-compatible providers
  - id: "def456"
    name: "My Provider"
    base-url: "https://api.example.com/v1"
    entries:
      - id: "ghi789"
        api-key: "sk-..."
    models:
      - name: my-model
        route: apikey

retry:
  max-attempts: 5
  initial-delay: 1s
```

## Development Conventions

- **Go**: Standard library `net/http` — no web framework. `httputil.ReverseProxy` for upstream forwarding.
- **Web UI**: Vanilla JavaScript — no frameworks, no build tools. Files are embedded via `go:embed web/*`.
- **Database**: SQLite via `modernc.org/sqlite` (pure Go, no CGO required for SQLite). CGO is only needed for the systray dependency.
- **JSON parsing**: `github.com/tidwall/gjson` for reading, `github.com/tidwall/sjson` for writing — no struct unmarshaling for API responses.
- **Logging**: `github.com/sirupsen/logrus` — all log messages use `[TAG]` prefixes (e.g. `[REQ]`, `[ROUTE]`, `[UPSTREAM]`, `[AUTH]`, `[ADMIN]`).
- **Concurrency**: `sync.RWMutex` for config, `golang.org/x/sync/singleflight` for token refresh dedup.
- **System tray**: `github.com/getlantern/systray` — requires main thread on macOS.
- **Config format**: YAML via `gopkg.in/yaml.v3`.
- **Token usage**: Parsed from streaming SSE (last `data:` line) or non-streaming response bodies. Three parsers for Claude/OpenAI/Gemini formats.
- **Retry**: Automatic retry on 429/502/503/529 with exponential backoff and `Retry-After` header support.
