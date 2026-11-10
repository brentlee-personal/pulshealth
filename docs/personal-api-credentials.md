# Personal API credential authority

The authorization module creates an independent opaque API bearer with
createAPICredential in web/lib/oauth/apiCredential.ts. It verifies a personal
grant's signature, issuer and configured resource, or looks up a live browser
session. Both require explicit permissions. It does not take an arbitrary
member ID for a session. Grant identity, client,
resource, version and the original permission snapshot must match storage.
The member is read from the parent account in the database.

Each bearer is 32 random bytes encoded as unpadded base64url. Only its
SHA-256 is stored in auth.api_credentials. The stored Ed25519 assertion has
audience family-health-api and the exact AuthenticatedPrincipal fields.
The assertion is not the bearer and is not accepted as a lookup secret.
It carries no legacy health:read scope. Only the authorization module holds
the signing key. The API reader receives the assertion and current principal
through auth.resolve_api_credential(bytea), without direct table access.

Grant-derived credentials allow only the same or narrower read operations,
types and UTC range. Their lifetime is at most 60 seconds. Session-derived
credentials may additionally permit data:export and last at most 30 minutes.
Both lifetimes are capped by the parent's expiry. A grant exchange also
cannot outlive the signed subject token. An API credential cannot
change or delete health data. New grants store their resolved resource even
when the authorization request omitted the resource indicator.

The database resolver checks the account, credential, parent state, parent
resource, version, scope and expiry on each statement. It uses statement
time rather than transaction-start time, so an open read transaction cannot
keep an expired credential alive. Session deletion removes its credentials
through an ownership foreign key. The existing OAuth cleanup job removes
expired or revoked credentials.

Migration 044 also seals old active grants whose resource was never stored.
Those grants require new consent. It preserves health records and accounts.
The updated role script grants the API reader only the resolver's EXECUTE
privilege in auth; it cannot read the credential table. Historical schema
baselining can provision the earlier role before this function exists.

The Go HTTP authenticator resolves the opaque bearer, verifies its Ed25519
assertion with local public keys, and compares the signed principal with
current storage. Requests also check operation, type and UTC bounds.
The Web health route creates a separate API credential after checking its
subject grant. It does not forward that subject or a shared service token.

Migration 045 adds credential-scoped security-barrier views in schema api.
The api_reader role can select those views, not health base tables or
Timescale storage relations. Each request opens a read-only READ COMMITTED
transaction and sets its credential hash locally. Each SQL statement checks
live authority. Rollback clears the identity before the connection returns
to the pool, including after request cancellation. Health aggregates use
scoped raw inputs. Unverified legacy profile and activity-day facts remain
sealed. This does not erase their stored records.

Catalog caches are keyed by member and opaque credential hash. Two credentials
for one member cannot share results. Background refreshes open a fresh scoped
transaction and recheck live authority before reading.

Raw-sample and workout streams read at most 256 rows per statement. Their
cursor uses the original database timestamp and UUID, preserving microsecond
order. Each page closes its database rows before checking the signature and
live parent again. A committed revocation stops the stream; a response that
already started aborts its transfer instead of ending as a complete file.

Integration authorization tests use real api_reader connections, signatures
and HTTP. Legacy calendar and SQL math tests use the separate read-only
grafana reference role through REFERENCE_DATABASE_URL. They do not prove
personal authorization. The fixture writer alone uses ADMIN_DATABASE_URL.

Family Health T07 remains incomplete. The full old configuration and
bootstrap chain, forced Web accounts, browser behavior,
and final regression and restore checks remain pending. The branch is not
approved for deployment or real health data.
