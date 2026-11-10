import { createPrivateKey, createPublicKey, type KeyObject } from "node:crypto";
import { readFileSync, statSync } from "node:fs";

export interface SigningKey {
  id: string;
  privateKey: KeyObject;
}

const KEY_ID = /^[A-Za-z0-9_-]{1,64}$/;
const PUBLIC_FIELDS = new Set(["kid", "kty", "crv", "x", "alg", "use"]);

function keyFile(path: string, privateKey: boolean): Buffer {
  const stat = statSync(path);
  if (!stat.isFile() || stat.size < 1 || stat.size > 16_384 || (privateKey && (stat.mode & 0o077) !== 0)) {
    throw new Error("Invalid key file");
  }
  return readFileSync(path);
}

/** Trusted local configuration, never a URL or a token-selected key source. */
export function readPublicKeys(path: string): ReadonlyMap<string, KeyObject> {
  try {
    const document: unknown = JSON.parse(keyFile(path, false).toString("utf8"));
    if (!document || typeof document !== "object" || !("keys" in document) ||
        Object.keys(document).length !== 1 || !Array.isArray(document.keys) ||
        document.keys.length < 1 || document.keys.length > 8) throw new Error("Invalid key set");
    const keys = new Map<string, KeyObject>();
    for (const raw of document.keys) {
      const key: unknown = raw;
      if (!key || typeof key !== "object" || Object.keys(key).some((field) => !PUBLIC_FIELDS.has(field)) ||
          !("kid" in key) || typeof key.kid !== "string" || !KEY_ID.test(key.kid) || keys.has(key.kid) ||
          !("kty" in key) || key.kty !== "OKP" || !("crv" in key) || key.crv !== "Ed25519" ||
          !("alg" in key) || key.alg !== "EdDSA" || !("use" in key) || key.use !== "sig" ||
          !("x" in key) || typeof key.x !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(key.x)) {
        throw new Error("Invalid public key");
      }
      const bytes = Buffer.from(key.x, "base64url");
      if (bytes.length !== 32 || bytes.toString("base64url") !== key.x) throw new Error("Invalid public key encoding");
      keys.set(key.kid, createPublicKey({ key: { kty: key.kty, crv: key.crv, x: key.x }, format: "jwk" }));
    }
    return keys;
  } catch {
    throw new Error("PULS_AUTH_PUBLIC_KEYS_FILE is unavailable or is not a public Ed25519 key set");
  }
}

/** Only the authorization module receives this owner-only PKCS8 file. */
export function readSigningKey(path: string, id: string, publicKeys: ReadonlyMap<string, KeyObject>): SigningKey {
  try {
    const published = publicKeys.get(id);
    if (!KEY_ID.test(id) || !published) throw new Error("Unknown signing key");
    const privateKey = createPrivateKey({ key: keyFile(path, true), format: "pem", type: "pkcs8" });
    if (privateKey.asymmetricKeyType !== "ed25519" || !createPublicKey(privateKey).equals(published)) {
      throw new Error("Signing key does not match the public key set");
    }
    return { id, privateKey };
  } catch {
    throw new Error("PULS_AUTH_PRIVATE_KEY_FILE or PULS_AUTH_KEY_ID is unavailable, invalid, or mismatched");
  }
}
