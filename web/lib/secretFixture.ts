// Synthetic file credentials only, with the same lifecycle as oauthKeyFixture.
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll } from "vitest";

export function secretFixture(values: Record<string, string | undefined>): Record<string, string> {
  const directory = mkdtempSync(join(tmpdir(), "puls-secret-fixture-"));
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(values)) {
    if (value === undefined) continue;
    const path = join(directory, name);
    writeFileSync(path, value + "\n", { mode: 0o600, flag: "wx" });
    env[name + "_FILE"] = path;
  }
  afterAll(() => rmSync(directory, { recursive: true, force: true }));
  return env;
}
