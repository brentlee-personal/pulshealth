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

This module does not complete Family Health T07. The Go HTTP authenticator
and Web forwarding still need to switch to these credentials. The API
reader's old health base-table grants, default identity, shared API bearer,
derived-query boundaries and streaming checks still require retirement or
enforcement. Module tests are not HTTP, browser, deployment or real-data
acceptance. Do not deploy the unfinished branch with family health data.
