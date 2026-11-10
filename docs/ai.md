# Use PulsHealth with AI assistants

Use an AI assistant to ask questions about data already synced to your
PulsHealth database. The read-only MCP server translates those questions into
product API requests; it cannot write to Apple Health or the database.

**Setting up a server first?** Follow [Set up your server](setup.md), which
includes a prompt for your coding assistant. [llms.txt](https://pulshealth.com/llms.txt)
is the documentation index. Giving an assistant that URL does not grant access
to your health data.

## Choose your connection

| Your situation | Start here |
|---|---|
| You use a hosted PulsHealth account | [Sign in to the hosted connector](#connect-an-ai-assistant-to-your-pulshealth-account) |
| Your assistant runs locally and can reach your server | [Local MCP or private HTTP connection](#two-ways-to-connect) |
| Your assistant connects from its provider's cloud | [Public HTTPS with account sign-in](#hosted-connectors-oauth-clients-that-sign-in-instead-of-taking-a-token) |
| You want to try one question without a connector | [Paste a summary](#no-mcp-at-all-paste-a-summary) |

A connection lets the assistant's provider process the data it reads, including
profile details. Choose the scope and provider deliberately. The iOS app syncs
to your database; it does not connect directly to AI.

## Connect an AI assistant to your PulsHealth account

If you already sync to a hosted PulsHealth account, add this remote MCP URL
in a client that supports OAuth sign-in:

```text
https://mcp.pulshealth.com/mcp
```

Connect, sign in with your PulsHealth account, review the consent page, and
choose **Allow**. Leave client ID and client secret fields empty when the
client supports automatic registration. A client that only accepts a static
token cannot use the hosted connection.

For Claude, use its custom remote connector flow. Available surfaces, menus
and plan requirements are described in [Claude's connector guide](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp).
For Claude Code:

```bash
claude mcp add --transport http --scope user pulshealth https://mcp.pulshealth.com/mcp
```

Run `/mcp`, select `pulshealth`, and authenticate. Then ask:
*“What data do you have about me, and how current is it?”*

The grant covers all health data and profile details on your account, read-only.
Revoke a connection under **AI assistants** on your
[account page](https://app.pulshealth.com/account). Refreshing stops immediately;
an already issued access token can remain valid for up to 30 minutes.
Changing your password or deleting the account also revokes its grants.
See the [privacy policy](https://pulshealth.com/privacy/).

## What is available today

| Ask about | Tool the assistant uses |
|---|---|
| Who has data on the server, if more than one person does | `list_users` |
| Which data exists, how current it is, what day it is | `list_available_types` |
| How the last week or month went, in one page | `get_summary` |
| Who the data belongs to (name, age, sex) | `get_profile` |
| Current weight, resting heart rate, HRV, VO2 max, blood oxygen, ... | `get_latest_metrics` |
| Daily steps, energy, distance, exercise minutes, heart-rate averages, weight trend, ... | `get_daily_metrics` |
| Activity rings and goals per day | `get_activity_rings` |
| Workouts, filtered by date and activity | `list_workouts` |
| One workout's heart rate / power / pace statistics, laps, pauses | `get_workout` |
| The second-by-second curves inside one workout | `get_workout_series` |
| Sleep by night: time asleep, time in bed, stages | `get_sleep` |
| The individual records of one type, raw | `get_samples` |
| Logged moods and emotions | `get_state_of_mind` |

Daily values are the deduplicated ones (no iPhone + Watch double counting),
every value carries its unit, and requested dates use the selected account's
reporting time zone. Phone aggregates and rings retain recorded phone calendar
dates, which can differ from raw reporting dates during travel. Sleep follows Apple Health: a night is dated by the day you
wake up, and where several devices recorded the same night nothing is summed
across them. `get_samples` is the one tool that returns undeduplicated
records; that is what makes it useful for looking at particular readings and
useless for totals. The assistant can also read `pulshealth://guide`, a short
manual on the data model and its traps, and two ready-made prompts
(`weekly_summary`, `compare_workouts`).

When several phones sync to one server, every tool takes an optional `user`
(a `user_id` from `list_users`); without it the assistant reads the API's
default person. The API only honours another user when its `PULS_MULTI_USER`
is on, and an MCP instance can be pinned to one person with `PULS_USER_ID`
(`PULS_MCP_USER_ID` for the Compose service) so that a connector you hand
to one household member can never be asked about another.

**Not yet:** GPS routes, medication doses, ECGs and heartbeat series. They
are in the database; no tool serves them.

For a whole range as a *file* rather than an answer in a chat (a spreadsheet,
a notebook, something to attach), use `GET /v1/export` or the `puls-export`
CLI instead of a tool call: [`export.md`](export.md).

## No MCP at all: paste a summary

After [server setup](setup.md), run this on the server from the repository root.
Load the trusted local settings without pasting tokens into a chat:

```bash
set -a
. server/.env
set +a
curl --fail-with-body -sS \
  -H "Authorization: Bearer $PULS_API_TOKEN" \
  'http://127.0.0.1:8081/v1/summary?range=7d'
```

Read the result before sharing it. It contains health information.
`range` accepts `7d`, `14d`, `30d` or `90d`; `format=json` returns the same
figures as JSON. The summary names its date range, time zone, units and
coverage. On a remote computer, replace the loopback address with your
reachable API URL or use an SSH tunnel.

## Two ways to connect

1. **Local binary (stdio):** your assistant launches `pulshealth-mcp`.
   It needs the product API URL and `PULS_API_TOKEN`. Keep the API private
   through a VPN or SSH tunnel; no public endpoint is required.
2. **Remote MCP (HTTP):** use the Compose `mcp` service with a reachable
   `/mcp` URL and `PULS_MCP_TOKEN`. The client must support bearer headers.
   Cloud clients need a public HTTPS endpoint; clients requiring sign-in
   also need [OAuth](#hosted-connectors-oauth-clients-that-sign-in-instead-of-taking-a-token).

The API is port **8081**; MCP is **8082**. Neither uses the phone's ingest
token. See [MCP configuration](../server/mcp/README.md#environment) for all settings.

### Get the binary

Only needed for stdio. With Go 1.26 or newer, run from the repository root:

```bash
mkdir -p "$HOME/.local/bin"
(cd server/mcp && go build -o "$HOME/.local/bin/pulshealth-mcp" .)
"$HOME/.local/bin/pulshealth-mcp" --version
```

Use the full binary path in your client configuration. There is no Go module
at the repository root. Build from the same revision as your server.

To reach a remote API privately, keep this command running in another terminal
(replace `user@server` with your SSH destination):

```bash
ssh -N -L 8081:127.0.0.1:8081 user@server
```

Then use `PULS_API_URL=http://127.0.0.1:8081` on your computer.

### Claude Desktop

For a local MCP configuration, use the JSON below in the client settings that
support launching a command. Replace the path and token locally; keep the
file out of version control. Desktop, chat and mobile support differ; consult
[Claude's local and remote connector guide](https://support.claude.com/en/articles/11725091-when-to-use-desktop-and-web-connectors).

```json
{
  "mcpServers": {
    "pulshealth": {
      "command": "/absolute/path/to/pulshealth-mcp",
      "env": {
        "PULS_API_URL": "http://127.0.0.1:8081",
        "PULS_API_TOKEN": "YOUR_API_TOKEN"
      }
    }
  }
}
```

### Claude Code

With `PULS_API_TOKEN` already loaded securely in your local shell:

```bash
claude mcp add --scope user \
  --env PULS_API_URL=http://127.0.0.1:8081 \
  --env "PULS_API_TOKEN=$PULS_API_TOKEN" \
  --transport stdio pulshealth -- "$HOME/.local/bin/pulshealth-mcp"
```

For an HTTP connector, replace the example hostname and load its MCP token:

```bash
claude mcp add --transport http --scope user \
  --header "Authorization: Bearer $PULS_MCP_TOKEN" \
  pulshealth https://mcp.example.com/mcp
```

Use one configuration for the name `pulshealth`. `claude mcp list` and `/mcp`
show its status. See [Claude Code's MCP guide](https://code.claude.com/docs/en/mcp)
for configuration and authentication commands.

### Cursor

Use the stdio JSON above in your private `~/.cursor/mcp.json`, adding
`"type": "stdio"` inside the `pulshealth` entry. Alternatively, configure
an HTTP connector (see [Cursor MCP configuration](https://cursor.com/docs/mcp)):

```json
{
  "mcpServers": {
    "pulshealth": {
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
    }
  }
}
```

### Remote connector: the Compose service over HTTPS

The bootstrap starts `mcp` with its own generated token. From the repository root:

```bash
docker compose -f server/docker-compose.yml ps mcp api
curl --fail-with-body -sS http://127.0.0.1:8082/healthz
```

Expect `{"api":true,"ok":true}`. Keep port 8082 on loopback and forward your
HTTPS endpoint to it. For an existing Tailscale network, one option is:

```bash
tailscale serve --bg --https=8445 http://localhost:8082
```

Use the HTTPS URL printed by Tailscale, with `/mcp` appended. This is private
to that network, so it works only when the client can join it.
[Server networking](../server/README.md#exposing-the-server) covers other proxies.
Set `TRUST_PROXY_HEADERS=true` only when a trusted proxy is the sole entry point.

The token presented by the client is `PULS_MCP_TOKEN`; the MCP service keeps
`PULS_API_TOKEN` for its own API calls. A health check verifies dependencies,
not client authentication. Finish by connecting the assistant and asking it
to list available data and its latest date.

### Hosted connectors (OAuth): clients that sign in instead of taking a token

For your own cloud-accessible connector, configure:

- The [viewer in accounts mode](../web/README.md#accounts-mode), with each
  person's account bound to their synced user ID.
- A public HTTPS viewer URL and a public HTTPS MCP URL. A private tailnet URL
  cannot be reached by a cloud client.
- Separate signing and public files, created with `make web-keys ARGS='key-v1'`. See [Signing keys](auth-keys.md), then configure file references in `server/.env`:

```dotenv
WEB_ACCOUNTS=true
WEB_PUBLIC_URL=https://viewer.example.com
PULS_AUTH_KEY_ID=key-v1
PULS_AUTH_PRIVATE_KEY_FILE=/auth-keys/key-v1.pem
PULS_AUTH_PUBLIC_KEYS_FILE=/auth-public/key-v1.json
PULS_MCP_URL=https://mcp.example.com/mcp
```

Compose passes `WEB_PUBLIC_URL` to MCP as `PULS_MCP_OAUTH_ISSUER`.
Recreate `web` and `mcp` using the same Compose files used for installation.
Verify `https://mcp.example.com/.well-known/oauth-protected-resource/mcp`
names your viewer, then connect and sign in through your assistant.

For users other than the API's default person, enable `PULS_MULTI_USER=true`
on the API. Also pin the static MCP credential with
`PULS_MCP_USER_ID=<intended-user-uuid>` so it cannot select every person.
OAuth calls always use the signed-in person's ID, regardless of that pin.

The grant is `health:read` over all of that person's data. Revocation stops
refreshes immediately; access tokens expire within 30 minutes. Rotating the
shared OAuth secret invalidates existing access tokens, but does not revoke
refresh grants. See the [viewer OAuth reference](../web/README.md#ai-assistants-oauth)
and [MCP security model](../server/mcp/README.md#oauth-resource-server).

## ChatGPT: the product API as a custom GPT Action

An existing private GPT Action can call the product API with its OpenAPI
schema and a bearer API key. Availability and configuration depend on the
client and workspace; see [OpenAI's current Action documentation](https://help.openai.com/en/articles/9442513-configuring-actions-in-gpts).
For a new connection, use an MCP-capable client where available, or paste a
summary. Actions require a public API URL reachable by the provider.

### 1. Publish the API on a public HTTPS URL

Forward a TLS proxy to `127.0.0.1:8081`. Keep the database private.
With a trusted proxy as the only entry point, set `TRUST_PROXY_HEADERS=true`
so the API uses its forwarded host and scheme. Check:

```bash
curl --fail-with-body -sS https://api.example.com/healthz
curl --fail-with-body -sS https://api.example.com/openapi.json
```

Verify `servers[0].url` names that public endpoint, not loopback.

### 2. Import it

Import `https://api.example.com/openapi.json` in the Action's schema editor.
Use the schema from your deployed server, which matches its API version.

### 3. Configure the token

Choose API key authentication with **Bearer**, and enter `PULS_API_TOKEN`
locally. This is the read token, not `PULS_TOKEN` or `PULS_MCP_TOKEN`.

### 4. Check it

Ask which data is available and how current it is. Check that the result names
the expected person and real sync dates. `latest` means the newest uploaded
record, not today's date. Ask the assistant to use `/v1/users` for the selected
person's reporting zone and `/v1/metrics/daily` for daily totals.

### Caveats

Keep the GPT private: anyone able to use its stored credential can read the
data it authorizes. With `PULS_MULTI_USER=true`, that credential can read every
person. Use short date ranges and JSON endpoints; use the export CLI for files.
When retiring the integration, rotate the API token, recreate `api` and `mcp`
with the installation's Compose files, and remove unneeded public routes.

## Try these

- **"What data do you have about me, and how current is it?"** One
  `list_available_types` call; a good first question, it also tells the
  assistant today's date.
- **"How have I been doing this month?"** One `get_summary` call with
  `range: 30d`; the same page `GET /v1/summary` serves, so it is also the
  thing to paste into a chat that has no connector.
- **"How did I sleep last week?"** One `get_sleep` call for the seven
  days; each row is a night, dated by the morning you woke up, with time
  asleep, time in bed and the core / deep / REM split in minutes. The
  `weekly_summary` prompt includes it.
- **"When exactly did my heart rate spike during yesterday's meeting?"**
  `get_samples` with `HKQuantityTypeIdentifierHeartRate` for that day; the
  individual readings, not a daily average.
- **"Show me how my heart rate moved through Saturday's run."**
  `list_workouts` for the day, then `get_workout_series` with the uuid.
- **"Compare my runs this month to last month."** Two `list_workouts`
  calls with `activity_type: running`, then totals, averages and pace;
  `get_workout` on a few for heart rate. The `compare_workouts` prompt does
  exactly this.
- **"Did I close my rings yesterday?"** `get_activity_rings` for one day;
  closed means value ≥ goal.
- **"What's my resting heart rate trend over the last 90 days?"**
  `get_daily_metrics` with `HKQuantityTypeIdentifierRestingHeartRate` and
  `HKQuantityTypeIdentifierHeartRateVariabilitySDNN`.
- **"How much did I weigh at the start of the year versus now?"**
  `get_latest_metrics` for now, `get_daily_metrics` for January.

## How the server keeps the model honest

Daily tools carry units and use calendar dates in the selected person's zone.
They prefer the phone's aggregates, with server rollups where supported;
raw samples remain undeduplicated. Missing data stays missing rather than
becoming zero. The embedded [assistant guide](../server/mcp/guide.md) explains
these rules and provides question-to-tool recipes.

## Security

- Read tokens expose health and profile data. Keep client configuration and
  `server/.env` private; use HTTPS for remote HTTP connections.
- An unpinned static MCP token can select other people only when the API
  enables multi-user reads. OAuth is always pinned to the signed-in account.
- Read-only access still sends returned data to the assistant's provider.
  Review its data-use settings before connecting.

## Troubleshooting

| Symptom | Check |
|---|---|
| API `401` in a tool result | MCP's `PULS_API_TOKEN` must match the API. |
| MCP `401` before tools load | The client's MCP token is wrong, or OAuth needs sign-in. |
| API unreachable or MCP health `503` | Check the API container, its database, and `PULS_API_URL`. |
| Cloud client cannot connect | Its provider must reach the public HTTPS MCP URL; a private VPN address is insufficient. |
| OAuth discovery `404` | Check all OAuth settings; the viewer also requires accounts mode. |
| API `403` after sign-in | The account differs from the default user and `PULS_MULTI_USER` is off. |
| No data or stale data | Confirm the selected user and a completed phone sync. The catalog can lag uploads because it is cached. |
| Wrong calendar day | Check the account's reporting zone. Rings and phone aggregates keep their recorded phone dates; travel can differ from raw-day grouping. |
| Time-zone lookup fails | Restore API connectivity; changing a fallback zone does not repair an unavailable API. |
| Local process fails to start | Run the same binary with the same environment; errors go to stderr. `--version` checks installation only. |
