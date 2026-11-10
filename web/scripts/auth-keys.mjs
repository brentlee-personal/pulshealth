#!/usr/bin/env node
// Run as the authorization service user; never prints or replaces key material.
import { generateKeyPairSync } from "node:crypto";
import { closeSync, fsyncSync, mkdirSync, openSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

export function createKeyFiles(privateDirectory, publicDirectory, id) {
  if (typeof id !== "string" || !/^[A-Za-z0-9_-]{1,64}$/.test(id)) throw new Error("Invalid signing key id");
  mkdirSync(privateDirectory, { recursive: true, mode: 0o700 });
  mkdirSync(publicDirectory, { recursive: true, mode: 0o755 });
  const pair = generateKeyPairSync("ed25519");
  const privateFile = join(privateDirectory, `${id}.pem`);
  const publicFile = join(publicDirectory, `${id}.json`);
  const document = { keys: [{ ...pair.publicKey.export({ format: "jwk" }), kid: id, alg: "EdDSA", use: "sig" }] };
  const created = [];
  try {
    for (const [path, bytes, mode] of [
      [privateFile, pair.privateKey.export({ type: "pkcs8", format: "pem" }), 0o600],
      [publicFile, JSON.stringify(document) + "\n", 0o644],
    ]) {
      const fd = openSync(path, "wx", mode);
      created.push(path);
      try { writeFileSync(fd, bytes); fsyncSync(fd); } finally { closeSync(fd); }
    }
    return { id, privateFile, publicFile };
  } catch {
    for (const path of created.reverse()) unlinkSync(path);
    throw new Error("Key files were not created; existing files were preserved");
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [privateDirectory, publicDirectory, id, extra] = process.argv.slice(2);
  if (!privateDirectory || !publicDirectory || !id || extra) {
    console.error("usage: node scripts/auth-keys.mjs <private-directory> <public-directory> <key-id>");
    process.exitCode = 2;
  } else {
    try { console.log(JSON.stringify(createKeyFiles(privateDirectory, publicDirectory, id))); }
    catch (error) { console.error(error instanceof Error ? error.message : "Key generation failed"); process.exitCode = 1; }
  }
}
