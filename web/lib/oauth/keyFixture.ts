// Synthetic credentials for tests only. RFC 8032 section 7.1 test 1 is public.
import { createPrivateKey, createPublicKey } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll } from "vitest";

export function oauthKeyFixture() {
  const id = "rfc8032-test-1";
  const d = Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url");
  const x = Buffer.from("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "hex").toString("base64url");
  const privateKey = createPrivateKey({ key: { kty: "OKP", crv: "Ed25519", d, x }, format: "jwk" });
  const publicKey = createPublicKey(privateKey);
  const directory = mkdtempSync(join(tmpdir(), "puls-oauth-fixture-"));
  const privateFile = join(directory, "private.pem");
  const publicFile = join(directory, "public.json");
  writeFileSync(privateFile, privateKey.export({ type: "pkcs8", format: "pem" }), { mode: 0o600 });
  writeFileSync(publicFile, JSON.stringify({ keys: [{ kty: "OKP", crv: "Ed25519", x, kid: id, alg: "EdDSA", use: "sig" }] }));
  afterAll(() => rmSync(directory, { recursive: true, force: true }));
  return {
    signingKey: { id, privateKey }, publicKey, publicKeys: new Map([[id, publicKey]]),
    env: { PULS_AUTH_KEY_ID: id, PULS_AUTH_PRIVATE_KEY_FILE: privateFile, PULS_AUTH_PUBLIC_KEYS_FILE: publicFile },
  };
}
