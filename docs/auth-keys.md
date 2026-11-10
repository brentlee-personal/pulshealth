# Authorization signing keys

The web authorization module signs access tokens with Ed25519. MCP receives
only a public JWKS file. Tokens use the fixed header fields `alg=EdDSA`,
`typ=at+jwt` and an active `kid`. An unknown algorithm, key, issuer or
audience is refused. A signature alone does not establish a live grant;
personal authorization and downstream API checks remain separate requirements.

## Create keys

Start the web service with OAuth disabled, then run this as its service user:

```sh
make web-keys ARGS='key-v1'
```

The command creates `/auth-keys/key-v1.pem` and `/auth-public/key-v1.json`.
It prints filenames only. The private PKCS8 file has mode 0600 and stays in
the web-only `auth_signing` volume. The `auth_public` volume contains only
public keys and is mounted read-only in MCP. Existing files are preserved;
a failed attempt removes only files it created.

Set these file references in `server/.env`, using private HTTPS origins:

```dotenv
PULS_AUTH_KEY_ID=key-v1
PULS_AUTH_PRIVATE_KEY_FILE=/auth-keys/key-v1.pem
PULS_AUTH_PUBLIC_KEYS_FILE=/auth-public/key-v1.json
WEB_PUBLIC_URL=https://viewer.example.com
PULS_MCP_URL=https://mcp.example.com/mcp
```

Recreate web and MCP with the same Compose files used for installation.
The retired `PULS_MCP_OAUTH_SECRET` must be removed. A nonempty old setting
is a startup error. MCP also refuses a private-key setting. Private bytes
are never placed in environment variables or the repository.

## Rotate or recover

Create `key-v2` with the same command. Change the key id and both file paths
to the new filenames, then recreate web and MCP. The new public file lists
only the new key. MCP rereads its configured public file for each JWT, so
removing a key from that file retires it without a verifier cache. Replacing
a file in place must use an atomic rename. Requests can be refused while
services use different key configurations.

An invalid signer configuration prevents web startup. An unavailable key
file returns a fixed 503 at authenticated read and MCP verification
boundaries. Restore a matching, approved key set; no shared-key fallback is
available. Signature checks do not replace grant revocation or expiry checks.

The test seed in the RFC 8032 fixtures is public and must never be deployed.
