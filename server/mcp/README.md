# PulsHealth MCP server

A read-only [Model Context Protocol](https://modelcontextprotocol.io) server
that lets AI assistants — Claude Desktop, Claude Code, Cursor, and any client
that speaks MCP — answer questions from your Apple Health data. It is one Go
binary that talks **only to the product API** (`server/api`): it never opens
a database connection, so the API's bearer token and read-only database role
remain the whole trust boundary, and the API's deduplicated daily surfaces
are what the model sees.

Start with [Connect your AI assistant](../../docs/ai.md) for client setup,
or [Set up your server](../../docs/setup.md) if you do not have a database yet.
This page is the MCP operator and developer reference. The normal Compose
installation already includes this service; building a binary is only needed
for a local stdio client or development.

## What it exposes

| Tool | Answers |
|---|---|
| `list_users` | Everyone with data on the server, which one is the API's default, whether `multi_user` reads are on, and which user this instance is pinned to (if any; a pinned instance lists only that user). |
| `get_summary(range?)` | `GET /v1/summary` as markdown text: the last 7d (default), 14d, 30d or 90d in under sixty lines — activity, heart, sleep, workouts, body, coverage. The cheapest first call for a broad question. |
| `list_available_types` | Every HealthKit type with data: unit, row counts, earliest/latest, plus today's date and the time zone. The natural first call for anything specific. |
| `get_profile` | Name, email, date of birth, age, biological sex. |
| `get_latest_metrics(types)` | Newest raw sample per quantity type. |
| `get_daily_metrics(types, start_date, end_date)` | One deduplicated value per local day: sums for cumulative types, averages for discrete ones. |
| `get_activity_rings(start_date, end_date)` | Move / Exercise / Stand values and goals per day. |
| `list_workouts(start_date?, end_date?, activity_type?, limit?, offset?)` | Workout summaries, newest first. |
| `get_workout(uuid)` | Per-type statistics, events, and multi-sport parts of one workout. |
| `get_workout_series(uuid, types?, max_points?)` | The second-by-second streams inside one workout, downsampled. |
| `get_sleep(start_date, end_date)` | One row per night — asleep, in bed and stage minutes — dated by the day of waking. |
| `get_samples(type, start_date, end_date, limit?, offset?)` | Individual records of one type, raw and undeduplicated. At most 31 days. |
| `get_state_of_mind(start_date, end_date)` | Logged moods: valence, classification, labels, associations. |

Resources: `pulshealth://guide` (the embedded [`guide.md`](guide.md), written
for the model: data model, units, the iPhone + Watch double-counting rule,
question→tool recipes) and `pulshealth://types` (the live catalog). Prompts:
`weekly_summary` and `compare_workouts`.

Every tool but `list_users` takes an optional `user` — a `user_id` from
`list_users` — and passes it to the product API as `user=`; omitted, the API
answers for its own `PULS_USER_ID`. Naming anyone else needs the API's
`PULS_MULTI_USER` on; otherwise the API's 403 reaches the model as a tool
error. Per-user answers carry `user_id` whenever a user was named or the
instance is pinned. Every tool is annotated read-only and idempotent.
`get_summary` is the one tool whose answer is markdown rather than JSON: the
product API renders the page and the tool passes it on verbatim.

Tool inputs and outputs use `YYYY-MM-DD` calendar days and ISO 8601 instants
in the selected person's reporting time zone. The server translates them to the product API's
epoch-millisecond, half-open ranges: an inclusive `start_date`…`end_date`
becomes `[start of start_date, start of the day after end_date)` in that
zone, DST included. API errors surface as tool errors carrying the HTTP
status.

## Running it

Two modes, one binary:

```bash
# stdio (default): for Claude Desktop, Claude Code, Cursor, ...
PULS_API_URL=https://<api-host>:8444 PULS_API_TOKEN=... ./pulshealth-mcp

# streamable HTTP at /mcp, for remote connectors; refuses to start without
# PULS_MCP_TOKEN or OAuth
PULS_API_URL=http://127.0.0.1:8081 PULS_API_TOKEN=... PULS_MCP_TOKEN=... ./pulshealth-mcp --http 127.0.0.1:8082
```

Build from the same checkout as your server with Go 1.26 or newer:

```bash
cd server/mcp
go build -o pulshealth-mcp .
./pulshealth-mcp --version
```

Run the examples above from that directory, or use the full binary path.
There is no Go module at the repository root. `--version` checks the binary,
not its API connection.

The Compose stack runs it as the `mcp` service on `127.0.0.1:8082` in HTTP
mode, pointed at `http://api:8081` over the internal network.

### Environment

| Variable | Meaning |
|---|---|
| `PULS_API_URL` | Base URL of the product API. Default `http://127.0.0.1:8081`; Compose sets `http://api:8081`. |
| `PULS_API_TOKEN` | The product API's bearer token (`PULS_API_TOKEN` in `server/.env`). Required. |
| `PULS_MCP_TOKEN` | The static bearer token MCP clients may present to `/mcp` in `--http` mode. Required in that mode unless OAuth is on, and refused if still `.env.example`'s `change-me`; ignored in stdio mode. |
| `PULS_AUTH_PUBLIC_KEYS_FILE` | Local public-only Ed25519 JWKS. Private signing files are refused. See [Signing keys](../../docs/auth-keys.md). |
| `PULS_MCP_URL` | OAuth: this server's public URL, path included (`https://mcp.example.com/mcp`) — the resource identifier, and the only `aud` a token may carry. https, or http for a loopback host. |
| `PULS_MCP_OAUTH_ISSUER` | OAuth: the authorization server's issuer, the viewer's public origin (its `WEB_PUBLIC_URL`; Compose passes that). OAuth is on when the secret or the URL is set, and then all three are required — a partial setting stops startup. The issuer alone leaves OAuth off. |
| `PULS_TIME_ZONE` | Legacy fallback IANA zone. A selected user's `timeZone` from `GET /v1/users?user=...` takes precedence, including for OAuth callers. The selected user’s zone is refreshed for every incoming tool, resource, or prompt request and reused only within that request. For older APIs without per-user zones, an explicit setting wins; otherwise the API's top-level `timeZone` is used, falling back to UTC when absent. |
| `TRUST_PROXY_HEADERS` | `--http` mode: whether the auth-failure limiter keys on the **last** `X-Forwarded-For` entry (the one a trusted proxy appended) instead of the TCP peer. Same switch, default (`false`) and spelling rule as ingest's and the API's: `true/false`, `1/0`, `yes/no`, `on/off`, any case; anything else stops startup. |
| `PULS_USER_ID` | Optional. Pins this instance to one person: every API request names that user, and a tool call naming anyone else is refused without asking the API. Empty (the default) leaves the choice to each call, falling back to the API's own default user. Compose sets it from `PULS_MCP_USER_ID`. It binds stdio and the static token only: an OAuth access token is always pinned to its own `sub`. |

### HTTP endpoints (`--http`)

- `POST/GET/DELETE /mcp` — the streamable HTTP transport, behind
  `Authorization: Bearer <PULS_MCP_TOKEN or an OAuth access token>`.
  Sessions idle for 30 minutes are dropped. A session belongs to the
  identity that opened it (the static token, or one signed-in person): a
  request on it with any other credential is a 403. With OAuth on, a 401
  carries `WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource/mcp"`
  (plus `error="invalid_token"` when a token was presented), which is how
  a client finds where to sign in. Failed guesses are throttled per client address exactly
  as on ingest and the API (`server/README.md`, "Rate limiting"): ten in a
  burst, then ten a minute (one every six seconds), answered `429` with `Retry-After` *before*
  the token is compared; a correct token is never throttled, and every
  failure is logged (without the token). Only a bearer that could be a
  guess is gated or charged: a request with no bearer (an OAuth client's
  discovery request) always gets its 401 challenge, and an access token
  whose signature verifies is checked *before* the limiter — admitted if
  valid, a `401 invalid_token` if expired or otherwise invalid, neither
  charged — because hosted connectors share their operator's egress
  addresses and would otherwise spend one budget for everyone behind them.
- `GET /.well-known/oauth-protected-resource` and
  `/.well-known/oauth-protected-resource/mcp` — with OAuth on only (404
  otherwise), unauthenticated, CORS-open: the RFC 9728 document naming this
  resource (`PULS_MCP_URL`), its authorization server (the issuer), the
  scope `health:read` and header bearer tokens.
- `GET /healthz` — unauthenticated; `{"ok":true,"api":true}` when the
  product API's own `/healthz` (which pings its database) answers, else 503.
  The image is distroless, so Compose's healthcheck runs the binary itself:
  `mcp healthcheck [addr]` GETs `/healthz` on loopback (default `:8082`) and
  exits 0 on a 200, 1 otherwise.

The SDK's DNS-rebinding guard (reject a loopback listener seeing a
non-loopback `Host`) is switched off on purpose: a TLS reverse proxy on the
same host — Tailscale Serve, Caddy, nginx — forwards exactly that shape to
`127.0.0.1:8082`, and the bearer token, which a rebinding page cannot
present, is the access control.

### OAuth (resource server)

Clients that use account sign-in connect through OAuth. This includes the
hosted PulsHealth connection and Claude Code's `/mcp` authentication flow. The web viewer in
accounts mode is the authorization server (`web/README.md`); this server is
the resource server and still never touches the database. An access token is
an Ed25519 JWT the viewer signs with its private key
(`oauth.go`): fixed `alg=EdDSA`, `typ=at+jwt` and an active `kid`, at most
4096 bytes, verified against its local public file, `iss` equal to the issuer,
`aud` equal to `PULS_MCP_URL`, `exp` not past and `iat` not ahead (60 s of
skew each way), `sub` a lower-case UUID, `scope` containing `health:read`.
Anything else is a 401 logged with the reason (never the token). It is
charged to the auth-failure limiter only when the signature did not verify;
a correctly signed token that is expired or otherwise invalid cannot be a
guess and is not charged, and a valid one is admitted even from a throttled
address.

A verified token acts for its `sub` and nobody else, through the same pin
`PULS_USER_ID` enforces: every API request names that user, a tool call
naming anyone else is refused before the API is asked, `list_users` returns
only that person's row, and the `pulshealth://types` resource is theirs.
The product API must allow it: unless `sub` is the API's `PULS_USER_ID`, that
needs `PULS_MULTI_USER=true` (and then pin the static token with
`PULS_MCP_USER_ID`, or it reads everyone). The static `PULS_MCP_TOKEN`
behaves exactly as without OAuth.

Tokens are not checked against the database, so a revoked grant or a
disabled account keeps reading until its access token expires — at most 30
minutes; the viewer refuses the refresh at once.

## Security notes

- **The tokens grant read access to health data**, including name, email
  and date of birth from the profile. `PULS_API_TOKEN` and `PULS_MCP_TOKEN`
  are separate secrets so the MCP-facing one can be rotated without touching
  other API consumers; generate each with `openssl rand -hex 32`.
- **Expose remote HTTP only through TLS with token or OAuth authentication.** The compose
  service binds to loopback; publish it only through an HTTPS-terminating
  proxy (`tailscale serve --bg --https=8445 http://localhost:8082`, or your
  reverse proxy). Plain HTTP puts the token on the wire.
- In stdio mode the client launches the binary; the token lives in the
  client's config file. Treat that file like `.env`.
- The server is read-only by construction: there is no code path that
  issues anything but `GET` to the product API, and no database credential
  is ever present.
- stdout is the stdio transport; all logging goes to stderr as JSON.

## Development

```bash
cd server/mcp
go vet ./... && go test -race ./...
go build -o pulshealth-mcp . && PULS_API_TOKEN=x ./pulshealth-mcp --version
```

Tests run against an `httptest` fake of the product API built from the
OpenAPI shapes in `server/api/openapi.json` (date mapping across DST, error
propagation, limits) plus an end-to-end pass over the SDK's in-memory
transport and the HTTP transport with the token, the OAuth verifier against
the shared contract's test vector and a table of forged and malformed tokens,
per-person scoping over HTTP, and session binding across identities. This module deliberately
takes one dependency beyond the standard library, the official
`github.com/modelcontextprotocol/go-sdk`.

When the product API's shapes change (`server/api/openapi.json`), update
`api.go`, the tool descriptions in `tools.go`, and `guide.md` in the same
change. Where the API pages, the client either passes the page through
(`list_workouts`, `get_samples` expose `limit`/`offset`) or follows it to
the end itself: `get_daily_metrics` walks `/v1/metrics/daily`'s
`nextOffset` (`APIClient.DailyMetrics`, page size `dailyPageSize`) and
returns the whole range as one answer, merging a metric split across two
pages.
