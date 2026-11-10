import { createPrivateKey, generateKeyPairSync, sign, verify } from "node:crypto";
import { chmodSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import { readPublicKeys, readSigningKey } from "./keys";

const directories: string[] = [];
function files() {
  const directory = mkdtempSync(join(tmpdir(), "puls-auth-key-test-"));
  directories.push(directory);
  return { privateFile: join(directory, "signing.pem"), publicFile: join(directory, "public.json") };
}

// Public RFC 8032 section 7.1 test 1, never a deployment credential.
const seed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60";
const publicHex = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a";
const publicJwk = {
  kid: "rfc8032-test-1", kty: "OKP", crv: "Ed25519", alg: "EdDSA", use: "sig",
  x: Buffer.from(publicHex, "hex").toString("base64url"),
};
function writeFixture() {
  const paths = files();
  const privateKey = createPrivateKey({ key: { kty: "OKP", crv: "Ed25519", x: publicJwk.x, d: Buffer.from(seed, "hex").toString("base64url") }, format: "jwk" });
  writeFileSync(paths.privateFile, privateKey.export({ type: "pkcs8", format: "pem" }), { mode: 0o600 });
  writeFileSync(paths.publicFile, JSON.stringify({ keys: [publicJwk] }));
  return paths;
}

afterEach(() => {
  for (const directory of directories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

describe("controlled Ed25519 keys", () => {
  it("loads only the signer private file and reproduces the independent RFC signature", () => {
    const paths = writeFixture();
    const publicKeys = readPublicKeys(paths.publicFile);
    const signing = readSigningKey(paths.privateFile, publicJwk.kid, publicKeys);
    const signature = sign(null, Buffer.alloc(0), signing.privateKey);
    expect(signature.toString("hex")).toBe("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b");
    const publicKey = publicKeys.get(publicJwk.kid);
    if (!publicKey) throw new Error("RFC public key is missing");
    expect(verify(null, Buffer.alloc(0), publicKey, signature)).toBe(true);
    expect(() => sign(null, Buffer.alloc(0), publicKey)).toThrow();
  });

  it("refuses private material, other algorithms, duplicate ids and noncanonical public keys", () => {
    const paths = writeFixture();
    for (const keys of [
      [{ ...publicJwk, d: seed }], [{ ...publicJwk, alg: "HS256" }], [{ ...publicJwk, crv: "Ed448" }],
      [{ ...publicJwk, kty: "RSA" }], [{ ...publicJwk, use: "enc" }], [publicJwk, publicJwk],
      [{ ...publicJwk, kid: "../key" }], [{ ...publicJwk, x: `${publicJwk.x}=` }],
      [{ ...publicJwk, x: publicJwk.x.slice(0, -1) + "p" }], [], Array(9).fill(publicJwk),
    ]) {
      writeFileSync(paths.publicFile, JSON.stringify({ keys }));
      expect(() => readPublicKeys(paths.publicFile)).toThrow("PULS_AUTH_PUBLIC_KEYS_FILE");
    }
  });

  it("refuses unknown, mismatched, wrong-curve and publicly readable signing keys", () => {
    const paths = writeFixture();
    const original = readFileSync(paths.privateFile);
    const publicKeys = readPublicKeys(paths.publicFile);
    expect(() => readSigningKey(paths.privateFile, "unknown", publicKeys)).toThrow("PULS_AUTH_KEY_ID");
    for (const pair of [generateKeyPairSync("ed25519"), generateKeyPairSync("ed448")]) {
      writeFileSync(paths.privateFile, pair.privateKey.export({ type: "pkcs8", format: "pem" }));
      expect(() => readSigningKey(paths.privateFile, publicJwk.kid, publicKeys)).toThrow("PULS_AUTH_PRIVATE_KEY_FILE");
    }
    writeFileSync(paths.privateFile, original);
    chmodSync(paths.privateFile, 0o644);
    expect(() => readSigningKey(paths.privateFile, publicJwk.kid, publicKeys)).toThrow("PULS_AUTH_PRIVATE_KEY_FILE");
  });

  it("reloads an atomically rotated public key set, without retaining the retired key", () => {
    const paths = writeFixture();
    const pair = generateKeyPairSync("ed25519");
    const nextKey = { ...pair.publicKey.export({ format: "jwk" }), kid: "rotated", alg: "EdDSA", use: "sig" };
    writeFileSync(`${paths.publicFile}.new`, JSON.stringify({ keys: [publicJwk, nextKey] }));
    renameSync(`${paths.publicFile}.new`, paths.publicFile);
    expect([...readPublicKeys(paths.publicFile).keys()]).toEqual([publicJwk.kid, "rotated"]);
    writeFileSync(`${paths.publicFile}.new`, JSON.stringify({ keys: [nextKey] }));
    renameSync(`${paths.publicFile}.new`, paths.publicFile);
    const current = readPublicKeys(paths.publicFile);
    expect(current.has(publicJwk.kid)).toBe(false);
    const active = current.get("rotated");
    if (!active) throw new Error("Rotated public key is missing");
    const message = Buffer.from("synthetic key rotation");
    expect(verify(null, message, active, sign(null, message, pair.privateKey))).toBe(true);
  });

  it("fails closed on missing, oversized and malformed files without returning their contents", () => {
    const paths = writeFixture();
    const marker = "synthetic-secret-input";
    for (const content of ["", marker, " ".repeat(16_385), "[]", '{"keys":[],"unexpected":1}']) {
      writeFileSync(paths.publicFile, content);
      try {
        readPublicKeys(paths.publicFile);
        throw new Error("Invalid key file was accepted");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect(String(error)).toContain("PULS_AUTH_PUBLIC_KEYS_FILE");
        expect(String(error)).not.toContain(marker);
      }
    }
    expect(() => readPublicKeys(`${paths.publicFile}.missing`)).toThrow("PULS_AUTH_PUBLIC_KEYS_FILE");
  });
});
