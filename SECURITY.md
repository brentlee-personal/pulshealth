# Security policy

PulsHealth moves personal health data from an iPhone to a server the user
chooses. Report vulnerabilities privately using the channel below.

For installation, use [Server setup](docs/setup.md); for deployment controls,
see [server operations](server/README.md). This policy explains reporting,
credential boundaries and the assumptions those instructions rely on.

## Supported versions

The **iOS app** ships from the App Store; the current version there is the
supported one, and a fix reaches users in the next store release. Report
against it even if you cannot build the source.

The **server stack** is released as versioned images (`CHANGELOG.md`). While
it is on 0.x, the latest release and `main` are supported: fixes land on
`main` and ship in the next release. The **Swift package and the protocol
tooling** are not released separately; `main` is their supported line.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository:

**https://github.com/PulsHealth/pulshealth/security/advisories/new**

That is the only reporting channel. Do not open a public issue, pull request,
or discussion for a security problem, and do not email individual maintainers.

A useful report includes:

- which component is affected (iOS app, `PulsHealthSync` package, ingest
  server, product API, MCP server, web viewer, Compose stack, database schema);
- the commit or version you tested;
- steps or a proof of concept that reproduces the problem;
- what an attacker gains (data read, data written or deleted, denial of
  service, code execution, and so on);
- whether you believe it is already being exploited.

## What to expect

This is a volunteer-maintained project.

- You should get an acknowledgement within **7 days**.
- You should get an initial assessment (accepted, needs more information, or
  not a vulnerability) within **14 days**.
- Accepted reports are fixed on `main`, shipped in the next server release or
  app update, and published as a GitHub Security Advisory that credits you,
  unless you ask not to be named. The advisory says which release fixes it.
- Please allow up to **90 days** before disclosing publicly. If a fix is
  taking longer, the maintainer will say so rather than go quiet.

## Scope

Everything in this repository is in scope, in particular:

- **Ingest server** (`server/ingest`): the phone-facing upload endpoint.
  Authentication bypass, parsing crashes, decompression or memory
  exhaustion, SQL injection, and anything that lets one bearer token read or
  modify data outside its intended reach.
- **Product API** (`server/api`) and **MCP server** (`server/mcp`): token
  handling, data exposure beyond the read-only role they are meant to have.
  With OAuth on, the MCP server in particular: accepting an access token
  whose signature, `alg`, `typ`, issuer, audience, expiry or scope is wrong;
  acting for anyone but the token's `sub` (a tool's `user` argument, a
  `list_users` answer naming someone else, or an MCP session reused under
  another identity); and any write.
- **iOS app and `PulsHealthSync`**: handling of the server URL and bearer
  token, including a pairing link (`puls://pair`) changing the server without
  the confirmation it is supposed to require, and the PulsHealth database's
  sign-in sheet: a payload it did not return filling the fields, or one it did
  being applied without Save & Apply; health data written anywhere
  the user did not ask for — the one intended case is an on-device export,
  staged in the app's temporary directory and handed to the share sheet, so an
  export that lingers, lands somewhere else, or carries the token or profile
  is in scope; data sent anywhere other than the configured server.
- **Compose stack and schema** (`server/docker-compose.yml`, `server/db/migrations`):
  defaults that expose a service or credential more widely than documented.
- **Web viewer** (`web/`): as deployed the documented way — bound to loopback
  or a private interface, or in accounts mode behind a TLS proxy. In accounts
  mode in particular: reading another account's records by any route;
  signing in without the password or a valid invite; a session that survives
  sign-out, a password change or an invite reset; cross-site request forgery;
  getting past the failed-sign-in throttle; an open redirect; any table
  holding per-user data that the `web_app` database role can read directly
  rather than through its per-user views; and, with sign-up requests on, any
  way to get an account, a user or a sync token without an administrator's
  approval, to mint or revoke a sync token for anyone but the signed-in
  person, to reach `/admin` or its database functions without an
  administrator's session, or to erase a user outside the self-service and
  operator-allowlisted personal-user boundary described below. Invited
  personal accounts and administrators may pair their own phones; approval
  of a sign-up request is not required for that path. With OAuth on (the viewer
  as the MCP server's
  authorization server): an authorization code or access token for an
  account other than the one that approved it; getting a code without the
  consent screen's Allow, or by cross-site request; a redirect to an address
  the client did not register; skipping PKCE; reusing a code or a rotated
  refresh token without the grant being revoked; a grant that survives
  revocation, a password change or reset, disabling or deletion for longer
  than the 30-minute access-token lifetime; a token carrying any scope but
  `health:read`; and a consent screen that misrepresents what is granted.
  With the public demo on (`WEB_DEMO_USER`): anything a demo session can
  change about the demo account (an AI assistant connected to it included),
  reaching any other account from one, and `/demo` replacing a session the
  browser already holds. See the notes below.

Out of scope:

- Vulnerabilities in upstream images and dependencies (PostgreSQL,
  TimescaleDB, Grafana, Next.js, Go modules). Report those upstream; a report
  here is welcome if the project pins a version with a known fix available.
- Deployments that diverge from the documentation, such as publishing the web
  viewer in open or basic mode, Grafana, or the database port on a public
  interface.
- Attacks that require an unlocked phone in hand, or a compromised server host.
- HealthKit behaviour (delivery latency, permission-sheet quirks). Those are
  bugs, not vulnerabilities; use the issue tracker.

## Threat model

What the design protects, from whom, and what it assumes. The section after
this one says how each piece holds up.

- **What is worth protecting:** the health records in the database, the
  identity snapshot beside them (name, email, date of birth), and the
  credentials that write them (ingest tokens) or read them (the API, MCP and
  viewer credentials, viewer sessions, OAuth refresh and access tokens, and
  the `PULS_MCP_OAUTH_SECRET` that signs the latter).
- **Who is assumed hostile:** anyone on the network path between the phone
  and the server, and anyone who can reach an exposed port: the ingest
  endpoint, in accounts mode the viewer, and with OAuth on the MCP server.
  They are expected to guess
  tokens and passwords, replay requests, forge `X-Forwarded-For` and other
  client-written headers, send oversized or malformed batches, and try
  cross-site requests against a signed-in browser. In accounts mode, other
  account holders are hostile to each other. With OAuth on, so is any OAuth
  client: registration is open (dynamic client registration), a client's
  name is whatever it says, and a client may try to phish a consent, steer
  a code to its own redirect, or replay a code or refresh token.
- **Who is trusted:** the operator and the host the stack runs on, the TLS
  proxy in front of it (it appends the client address the limiters key on),
  the unlocked phone and its Keychain, an AI assistant a person approved
  (it reads everything that person's grant covers, and its provider receives
  it under the provider's terms — that is the point of the grant), and
  whoever holds the shared
  `PULS_TOKEN` (writes as any user) or the API and MCP tokens (read every
  user once `PULS_MULTI_USER` is on): those reach every user by design.
- **What it assumes:** TLS from the phone, the browser and an MCP client to
  the proxy; every service other than ingest, the accounts-mode viewer and
  (with OAuth on) the MCP server bound to loopback or a private network; and real secrets in `.env`, which the services check
  (none starts on `change-me`).
- **What a breach costs:** a leaked per-device token writes and deletes one
  user's data until revoked; a leaked shared token, every user's; a leaked
  API or MCP token reads what that service reads; a leaked OAuth access
  token reads one person's data for at most 30 minutes, and a leaked refresh
  token until the grant is revoked or the legitimate client next refreshes
  (reuse of a rotated token revokes the grant); a leaked
  `PULS_MCP_OAUTH_SECRET` mints access tokens for any user, reading what the
  MCP server reads, until it is rotated; a
  compromised viewer container in accounts mode reads every user's records
  and can act destructively on self-service or explicitly allowlisted personal
  users only (below); a compromised server host,
  everything.

## Things to know about the current design

These are documented properties of the current design; what is still open is
in `docs/roadmap.md`. They are not vulnerabilities to report; they are context
for judging what is.

- **Self-hosted.** PulsHealth is software you run; no PulsHealth service
  receives your data unless you choose one. Where your server runs, how it is
  exposed, and who can reach it are your decisions. The maintainer runs one
  instance (the viewer at `app.pulshealth.com`) for family and friends and
  for people whose access request they approve. The app offers it as the
  **PulsHealth database**, its only built-in destination, reached by
  signing in from Sync → Database: an `ASWebAuthenticationSession` on the
  phone connection page, whose Connect this iPhone returns an ordinary pairing code.
  The app holds that one address and nothing else about the instance. A
  report about the software covers it too, and one about that instance's
  configuration is welcome through the same channel.
- **Bearer tokens.** The ingest server accepts two kinds. The shared
  `PULS_TOKEN` is a single static value: anyone who holds it can upload,
  delete, and (via the reconciliation endpoints) enumerate samples for *any*
  user, because with it the `X-User-ID` header selects the user without
  further authentication. Per-device tokens (`make devices`) are stored only
  as a SHA-256, bound to one user — a request naming another is refused with
  403 — revocable one at a time and stamped with their last use, so a lost
  phone costs one `revoke`. The shared token stays enabled by default so an
  existing install is unchanged; `PULS_ALLOW_SHARED_TOKEN=false` (or an empty
  `PULS_TOKEN`) turns it off, and the `X-User-ID` hole exists only while it
  is on. Failed authentications are rate-limited per client IP, which slows
  guessing but does not change what a leaked token grants. Behind a proxy
  (`TRUST_PROXY_HEADERS=true`) the client is the **last** `X-Forwarded-For`
  entry, the one the trusted proxy appended; proxies append, so the first is
  whatever the client wrote.
- **Trust boundaries.** While `PULS_ALLOW_SHARED_TOKEN` is on, the shared
  `PULS_TOKEN` makes `X-User-ID` unauthenticated tenant selection: its holder
  writes and reconciles as any user. `PULS_API_TOKEN` and `PULS_MCP_TOKEN`
  read every user once `PULS_MULTI_USER` is on, and only the API's
  `PULS_USER_ID` while it is off. Ingest, the product API and the MCP server
  refuse to start on a token of `change-me`, and the migrate service refuses
  a database role password of `change-me`, so the `.env.example`
  placeholders cannot become live secrets.
- **Tokens live on the phone.** Each saved receiver/user has a separate
  Keychain item; only the active receiver is used for sync. Unpairing removes
  the local credential and progress, not remote data or server-side token
  authorization. Tokens are accessible
  after the first unlock so background syncs still run, and the sync-state and
  log files carry file protection and are excluded from device backups. If a
  Keychain write fails the app parks the token in that protected state file
  instead of dropping it — losing it would stall syncing until the user
  re-entered it — and removes it once the Keychain accepts it.
- **Medication aliases are installation-local.** The protected, backup-excluded
  `medication-identities.json` stores secure archives of opaque HealthKit concept
  identifiers and random UUID aliases, without names or dose events. Equality of
  decoded identifiers resolves an alias; archive bytes, object descriptions and
  display names never define identity. Only UUID aliases leave the device. Sync
  and throwaway exports share this registry, never sync progress. A corrupt,
  unreadable or unwritable registry fails the page/export rather than minting
  replacement IDs. Unpair/reset retains it; uninstall or file loss starts new IDs.
- **App Store financial alerts are independent of health records.** Optional
  accounts-mode V2 webhooks verify Apple signatures, bundle/app identifiers and
  environment using Apple's server library and public trust roots. They do not
  grant access or join transactions to health accounts. Only event hashes and
  delivery timestamps persist; email excludes buyer identity and transaction
  identifiers. Sandbox purchases are suppressed; TEST messages are labeled.
- **Complimentary Pro is device-local.** A separate device-only Keychain item
  holds only fixed gift start/end dates and a welcome acknowledgement. Verified
  original app builds below 23 qualify; no purchase is started. Failed
  verification or storage cannot create a grant, and expired records remain
  to prevent restarting it. This is not account-wide anti-abuse enforcement.
- **Analysis summaries are derived numbers, never samples.** Analyzing a
  type on the Explore tab stores one small file per type (counts, dates,
  per-day counts, a value histogram and percentiles, and per-source and
  per-device counts by name) under the same file protection and backup
  exclusion as the sync state. A summary file that carried an individual
  sample, a value paired with its timestamp, a sample identifier or any
  metadata would be a bug in scope here.
- **An ordinary export is a plain file, and it is yours once shared.** The app can
  write the selected health data to CSV, JSON or JSONL without a server
  (`PulsHealthSync/Sources/PulsHealthSync/Export/`). The files are staged in
  the app's temporary directory — never backed up — and deleted at the next
  launch and once the share sheet is done with them; they carry no token, no
  server URL and no name, e-mail or date of birth — of the app's own
  identifiers only the user ID, the install's random device ID and the phone's
  time zone — though the health data itself names the app or device that
  recorded each sample, as HealthKit does. They are not encrypted
  beyond iOS file protection, and after the share sheet hands them to Files,
  AirDrop or another app, where they rest is outside the app's control.
- **TLS is yours to provide.** Every service binds to loopback by default. The
  phone normally reaches ingest over HTTPS through a TLS-terminating
  proxy, optionally over a VPN. The explicit `--lan` setup permits plain HTTP
  to local-network hosts on a trusted LAN; it does not encrypt the token or
  health data. A bearer token is not a substitute for transport encryption.
- **The web viewer has three modes** (`web/README.md`, "Access control").
  Open, with no login, for loopback only; basic, one shared
  `WEB_AUTH_PASSWORD` over HTTP Basic, behind which everyone sees every user —
  in both, the bind address is the primary access control, so keep
  `WEB_BIND_ADDR` on loopback or a private network; and accounts
  (`WEB_ACCOUNTS=true`), with invite-only accounts and HTTPS required.
- **Accounts mode: what the database enforces, and what it does not.** The
  viewer connects as `web_app`, which has no grant on any table holding
  health data and reads it only through security-barrier views filtered on a
  transaction-local setting (`server/db/migrations/015_web_accounts.sql`;
  views, not row-level security, which TimescaleDB refuses on compressed
  hypertables). That turns a query that forgets its user filter into a
  harmless one. It does not make a compromised viewer harmless: code running
  as `web_app` — an SQL injection, a compromised container — can set the
  setting to any user, and can read the account table (email addresses,
  scrypt password hashes, session hashes), which sign-in needs. Shared,
  non-health metadata is readable by every account's role: the list of
  HealthKit type identifiers seen on the server, and TimescaleDB catalog
  information such as approximate row counts and chunk time ranges, reachable
  only with arbitrary SQL. Failed sign-ins are throttled in process, per
  address and per email, and reset when the container restarts; behind a
  proxy the address is `WEB_CLIENT_IP_HEADER`'s last entry
  (`cf-connecting-ip` behind Cloudflare), never a client-chosen first one.
  Password recovery uses an emailed, single-use 30-minute token stored only
  as SHA-256 (`028_password_recovery.sql`). Unknown, disabled and demo accounts
  receive the same public response. Durable hashed-IP/email counters survive
  restarts; a password credential snapshot invalidates stale links. Completing
  a reset locks the account, consumes its recovery links, replaces the password,
  removes browser sessions and revokes OAuth grants atomically. Existing OAuth
  access tokens expire within 30 minutes; iPhone sync tokens are unchanged.
  Expired links and counters are pruned hourly. The operator's invite remains
  a fallback when recovery email is unavailable.
- **Accounts mode's privileged steps are database functions.** Approving a
  request (which creates a user), minting or revoking a sync token,
  disabling an account, deleting your own, and purging a user are
  `SECURITY DEFINER` functions in schema `auth`
  (`server/db/migrations/016_web_signups.sql`); `web_app` may run exactly
  these, and still has no write grant on `users`, `device_tokens` or
  `auth.self_service_users`. Each takes the caller's session (its SHA-256,
  as `auth.sessions` stores it) and acts for that account. That is not a
  barrier against a compromised viewer: `web_app` writes `auth.sessions` to
  sign people in, so it can forge a session for any account. The barrier is
  that destructive functions act only on **self-service** users, those an
  approved request created (`auth.self_service_users`), or invited personal
  users explicitly enrolled by the operator through `WEB_PERSONAL_USERS`
  (`auth.personal_users`). The allowlist is not writable by `web_app`.
  Default-user, administrator and configured demo records remain protected.
  Phone pairing works for every enabled personal account's own user, including
  invited users and administrators (021_personal_device_pairing.sql). SQL as
  `web_app` can therefore mint/revoke their sync tokens and modify viewer
  accounts, but cannot erase protected household health data. The configured public demo cannot issue/list/revoke tokens,
  even through forged sessions: `auth.device_pairing_policy`, writable only
  by the operator and set by 022 from `WEB_DEMO_USER`, excludes it. Existing
  data reads remain scoped through the security-barrier views. The viewer shows a
  minted token once, as a pairing code, and keeps only its hash. The sign-up
  form creates nothing but a request and emails only the operator, so it
  cannot open the database to anyone or be used to mail a stranger; no
  address it handles is written to the log.
- **Account deletion and restored backups.** `027_account_deletion.sql`
  accepts only an eligible account's session. The web handler writes an atomic,
  fsynced UUID/request-date file on the independent deletion-ledger volume
  before committing revocation and the erasure queue; failure aborts the
  request. It tries the purge immediately, and a TimescaleDB worker retries
  pending removals every minute. Purge removes raw samples, routes/series,
  aggregates/rollups, profile, device tokens, accounts and their cascading
  sessions/invites/OAuth/recovery records, and scrubs unused source names.
  A random 32-byte receipt gives only pending/completed status; its hash and
  status record expire 30 days after completion. A minimal UUID/date tombstone
  and independent filesystem record remain indefinitely to suppress restores.
  The general purge function is not callable by `web_app`; authorized wrappers
  enforce eligibility. A compromised viewer can forge sessions for accounts
  within that boundary, as described above. Never restore or roll back the
  ledger alongside an older health snapshot. Hosted restore requires a
  current ledger and replays deletions after migrations before services reopen.
- **The public demo.** With `WEB_DEMO_USER` set, `/demo` signs anyone into
  one operator-made account holding sample data, with no password
  (`web/README.md`, "Demo account"). It is view-only by refusal in every
  handler that changes an account, not by hidden buttons; its sessions last
  two hours without sliding and store no IP address or browser name; it
  never replaces a live session; it is refused for an administrator's
  account, a sign-up-made user and the household's default user.
- **AI assistants over OAuth.** In accounts mode, with
  `PULS_MCP_OAUTH_SECRET` and `PULS_MCP_URL` set, the viewer is an OAuth 2.1
  authorization server for the MCP server, so a person can connect an
  assistant (a claude.ai custom connector, Claude Code, any MCP client) by
  signing in and approving a consent screen. Off, every OAuth path is 404.
  Clients register themselves (RFC 7591; rate-limited per address; HTTPS or
  loopback redirect URIs only, the loopback port ignored as RFC 8252
  allows); PKCE S256 is required; codes live five minutes and are single
  use, and a second use revokes the grant made from the first; refresh
  tokens rotate on every use, and presenting a rotated one revokes the
  grant. Codes, refresh tokens and client secrets are stored only as
  SHA-256. The access token is a 30-minute HS256 JWT (`aud` the MCP URL,
  scope `health:read`) that the MCP server verifies by itself — it still
  never talks to Postgres — so **revocation is not instant**: revoking on
  the account page, a password change or reset, disabling or deleting the
  account refuses the next refresh at once, but an access token already
  issued keeps working until it expires, at most 30 minutes later. The
  grant is read-only and covers one person: the MCP server sends every API
  call with `user=` the token's `sub` and refuses any other. To read a user
  other than the API's default, the product API needs `PULS_MULTI_USER=true`,
  which also lets `PULS_API_TOKEN` and `PULS_MCP_TOKEN` read every user —
  the static MCP token keeps working beside OAuth and is not bound to
  anyone. A client's name on the consent screen is self-reported and marked
  as such; the redirect host shown beside it is the part the viewer
  enforces. What an approved assistant reads leaves the stack for that
  assistant's provider, under the provider's terms; revoking stops further
  reads, not what was already read. `PULS_MCP_OAUTH_SECRET` is shared by
  the viewer and the MCP server and must be at least 32 characters (neither
  starts on `change-me`); anyone holding it can mint tokens for any user.
- **Health data at rest.** The database holds identifiable data (name, email,
  date of birth, sex) alongside samples. Ingest connects as the scoped
  DML-only `ingest` role, which cannot create or drop objects; set
  `INGEST_DB_USER=postgres` to fall back to the superuser. Backups are opt-in
  and off by default: enable the `backup` Compose profile, and run the restore
  drill in `server/README.md` yourself, because nothing else verifies that
  your dumps restore. A hosted retention commitment requires
  `PULS_BACKUP_STRICT_RETENTION=true` and a positive `PULS_BACKUP_KEEP_DAYS`:
  strict mode expires even the newest dump and checks at least hourly between
  dump runs, including after failed dumps. Active dumps, an unavailable service
  or storage failure can delay cleanup; monitor the oldest archive and apply
  the same policy to offsite copies. Do not infer a deployed retention period
  from defaults. Hosted restore requires `--deletion-ledger-ready`, validates
  the current independent ledger before destructive restore, applies migrations
  and replays deletion records before services reopen. See `server/README.md`
  for the operational sequence.

## One-time Data Requests

Request links are untrusted, self-contained instructions, not signed invitations
or study consent. The app bounds and validates the payload, supports fixed past
dates, and shows the full recipient before Generate & Send. Opening never reads
health data. Request generation uses an isolated export engine and fresh user
and device IDs; source/sample metadata can still identify a person. The manifest
includes the request. No configured credentials or profile are included.

Delivery accepts HTTPS only, without URL credentials, query parameters, cookies
or redirects. Responses are limited to 16 KiB and must acknowledge the submission
ID and ZIP checksum. The receipt proves only that this endpoint acknowledged the
file. Retry preserves the submission ID; receivers must enforce idempotency.
Request IDs and links are not authentication. Receiver operators must implement
access controls, storage protection, retention and any study consent themselves.
The reference sync ingest endpoint is not a request ZIP receiver. See
[the request contract](docs/requests.md).

## Hosted operating procedures

The [hosted operations runbook](docs/hosted-operations.md) defines operator access, deletion escalation, backup expiry, restore-ledger checks and incident handling. The hosted service is not end-to-end encrypted; database administrators remain trusted. Per-account views protect viewer access, not against a compromised application or operator. Late diagnostic writes hold the user lock and require an existing owner, so they cannot recreate account diagnostics after erasure.

Batch rejection diagnostics use the authenticated credential's owner, even when
a conflicting `X-User-ID` is refused. HTTP failures, batch logs and durable
rejection records use fixed failure categories rather than raw parser/database
errors. Correlation identifiers are UUID-validated; diagnostic wake triggers
and encodings are allowlisted. Arbitrary batch labels stay out of the batch
logger. This does not sanitize historical log files or previously saved
diagnostics, and does not change the health records stored by a valid upload.

## Optional iPhone account access

Settings → User signs in independently of Health sync. A same-origin protected
browser POST issues a five-minute one-use code with a SHA-256 PKCE challenge;
the handoff is bound to the live authorizing browser session and cascades away
on its revocation or password reset. Exchange locks the account before a fresh
session check, serializing it with resets. The fixed `puls://account` callback carries that code and state, never a session
credential. The iPhone checks state and exchanges the code with its verifier.
The resulting normal account session is stored in a separate device-only
Keychain item, never in sync state. Native profile calls use only a bearer
session over HTTPS, ignore cookies, and refuse redirects. The explicit managed
server setup action also uses this bearer at POST /api/native/connect, which
issues an ordinary scoped device token through the existing connectPhone path.
It accepts no caller identity and refuses demo sessions. The app validates the
returned user and PulsHealth domain before staging the pairing for Save & Apply.
Database-scoped reads
and `auth.set_my_profile` authorize the session's own user; demo mutations are
refused. Login email edits require the current password and invalidate pending
recovery and invitation links. Account UUIDs cannot be edited. Deploy migration
029 and the 100 grant script, then the viewer, before shipping the client.

- **App Store customer review alerts are operator-only.** The hourly host job
  keeps Apple API keys inside the configured credential container, validates
  pagination origin/path and refuses redirects. It emails only public review
  content using the existing SES sender, with no health database access.
  Private state records review IDs rather than bodies; failed sends retry.
  Keep configuration, state and credentials outside Git with restricted modes.
