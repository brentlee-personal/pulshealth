import { chmodSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, expect, it } from "vitest";
import { readSecret } from "../scripts/secrets.mjs";
import { secretFixture } from "./secretFixture";

const credential = "synthetic-private-value";
const env = secretFixture({ DATABASE_URL: credential });
const path = env.DATABASE_URL_FILE;

describe("protected credential files", () => {
  it("loads the exact private value and accepts an absent optional setting", () => {
    expect(readSecret("DATABASE_URL", env)).toBe(credential);
    expect(readSecret("OPTIONAL", {})).toBeUndefined();
  });

  it("refuses plaintext values even alongside a file", () => {
    for (const candidate of [{ DATABASE_URL: credential }, { ...env, DATABASE_URL: credential }]) {
      expect(() => readSecret("DATABASE_URL", candidate)).toThrow(/DATABASE_URL_FILE/);
      expect(() => readSecret("DATABASE_URL", candidate)).not.toThrow(/synthetic-private-value/);
    }
  });

  it("refuses empty, oversized, invalid UTF-8 and multiline contents", () => {
    for (const bytes of [Buffer.alloc(0), Buffer.alloc(8193, 120), Buffer.from([255]), Buffer.from("a\nb"), Buffer.from("a\r"), Buffer.from("a\0b")]) {
      writeFileSync(path, bytes);
      expect(() => readSecret("DATABASE_URL", env)).toThrow(/private secret file/);
    }
    writeFileSync(path, credential);
  });

  it("refuses public, group-readable and unreadable files", () => {
    for (const mode of [0o644, 0o640, 0o000]) {
      chmodSync(path, mode);
      expect(() => readSecret("DATABASE_URL", env)).toThrow(/private secret file/);
    }
    chmodSync(path, 0o600);
  });

  it("refuses links, directories, relative and missing paths without disclosing them", () => {
    const link = join(dirname(path), "link");
    symlinkSync(path, link);
    for (const candidate of [link, dirname(path), "relative-path", join(dirname(path), "private-missing-location")]) {
      const lookup = () => readSecret("DATABASE_URL", { DATABASE_URL_FILE: candidate });
      expect(lookup).toThrow(/private secret file/);
      expect(lookup).not.toThrow(/private-missing-location/);
    }
  });
});
