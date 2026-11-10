# Personal read permissions

Consent requires one or more operations, data types, and a UTC time range.
The form selects summaries by default. Raw samples require a separate choice.
The time range includes its start and excludes its end.
Data type names are exact identifiers, not wildcard rules.

Migration 042 adds permissions to authorization codes and grants, plus a
grant authorization version. Existing grants are revoked and unused codes
are removed. Existing health records and accounts are preserved. A person
must consent again. Do not roll an old image back against the new schema
as a substitute for a verified recovery instance.

The authorization code stores the chosen permission object. Exchanging it
copies that object into the grant. Refreshing preserves the current grant
permissions and version. A legacy code or grant without permissions cannot
be exchanged or refreshed. The signed access token carries the member,
grant credential, read operations, exact types, time bounds and version.
Tokens are bounded to 16384 bytes on both Node and Go verification paths.

This document describes consent and signing. It does not establish that a
reader enforces those claims. Family Health T07 remains open until the
personal API, Web, database roles, legacy-mode retirement and live revocation
checks pass together. MCP service authorization and token exchange remain
subject to T13. No real health data or production deployment is accepted by
these module checks.
