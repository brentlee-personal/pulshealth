import { sign, verify } from "node:crypto";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import { createKeyFiles } from "../../scripts/auth-keys.mjs";
import { readPublicKeys, readSigningKey } from "./keys";

const directories: string[] = [];
function directoriesForKeys() {
  const root = mkdtempSync(join(tmpdir(), "puls-key-generation-"));
  directories.push(root);
  return { privateDirectory: join(root, "private"), publicDirectory: join(root, "public") };
}
afterEach(() => {
  for (const directory of directories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

describe("authorization key generation", () => {
  it("runs the actual CLI with explicit paths and emits filenames only", () => {
    const paths = directoriesForKeys();
    const cli = spawnSync(process.execPath, ["scripts/auth-keys.mjs", paths.privateDirectory, paths.publicDirectory, "cli-key"], {
      cwd: process.cwd(), encoding: "utf8", timeout: 10_000,
    });
    expect(cli.status).toBe(0);
    expect(JSON.parse(cli.stdout)).toEqual({ id: "cli-key", privateFile: join(paths.privateDirectory, "cli-key.pem"), publicFile: join(paths.publicDirectory, "cli-key.json") });
    const missing = spawnSync(process.execPath, ["scripts/auth-keys.mjs"], { cwd: process.cwd(), encoding: "utf8", timeout: 10_000 });
    expect(missing.status).toBe(2);
    expect(missing.stdout).toBe("");
  });
  it("creates owner-only private keys and public-only verification files with fresh entropy", () => {
    const paths = directoriesForKeys();
    const first = createKeyFiles(paths.privateDirectory, paths.publicDirectory, "first");
    const second = createKeyFiles(paths.privateDirectory, paths.publicDirectory, "second");
    expect(statSync(first.privateFile).mode & 0o777).toBe(0o600);
    const publicKeys = readPublicKeys(first.publicFile);
    const signer = readSigningKey(first.privateFile, first.id, publicKeys);
    const publicKey = publicKeys.get(first.id);
    if (!publicKey) throw new Error("Generated public key is missing");
    const message = Buffer.from("synthetic generated-key check");
    expect(verify(null, message, publicKey, sign(null, message, signer.privateKey))).toBe(true);
    expect(readFileSync(first.privateFile).equals(readFileSync(second.privateFile))).toBe(false);
    expect(readFileSync(first.publicFile, "utf8")).not.toContain('"d"');
  });

  it("preserves existing files and removes only files created by a failed attempt", () => {
    const paths = directoriesForKeys();
    const original = createKeyFiles(paths.privateDirectory, paths.publicDirectory, "existing");
    const originalPrivate = readFileSync(original.privateFile);
    const originalPublic = readFileSync(original.publicFile);
    expect(() => createKeyFiles(paths.privateDirectory, paths.publicDirectory, "existing")).toThrow("existing files were preserved");
    expect(readFileSync(original.privateFile).equals(originalPrivate)).toBe(true);
    expect(readFileSync(original.publicFile).equals(originalPublic)).toBe(true);
    const blockedPublic = join(paths.publicDirectory, "blocked.json");
    writeFileSync(blockedPublic, "synthetic existing public file");
    expect(() => createKeyFiles(paths.privateDirectory, paths.publicDirectory, "blocked")).toThrow("existing files were preserved");
    expect(existsSync(join(paths.privateDirectory, "blocked.pem"))).toBe(false);
    expect(readFileSync(blockedPublic, "utf8")).toBe("synthetic existing public file");
    expect(() => createKeyFiles(paths.privateDirectory, paths.publicDirectory, "../escape")).toThrow("Invalid signing key id");
  });
});
