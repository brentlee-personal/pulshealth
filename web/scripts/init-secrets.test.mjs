import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createPublicKey } from "node:crypto";
import { chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { initializeSecrets, validateSettings } from "./init-secrets.mjs";

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "puls-init-check-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  chmodSync(root, 0o755);
  mkdirSync(join(root, "secrets"), { mode: 0o755 });
  const settings = join(root, "settings.env");
  const data = join(root, "pgdata");
  mkdirSync(data);
  writeFileSync(settings, "PULS_TIME_ZONE=UTC\nWEB_ACCOUNTS=true\n", { mode: 0o600 });
  return { root, settings, data, initialize: () => initializeSecrets(join(root, "secrets"), data, settings) };
}

test("generated files have separate roles, exact owners and private modes", t => {
  const f = fixture(t);
  f.initialize();
  for (const [role, uid, databaseRole] of [["api", 65532, "api_reader"], ["ingest", 65533, "ingest"], ["web", 1001, "web_app"]]) {
    const path = join(f.root, "secrets", role, "DATABASE_URL");
    assert.equal(lstatSync(path).uid, uid);
    assert.equal(lstatSync(path).mode & 0o777, 0o600);
    const url = new URL(readFileSync(path, "utf8").trim());
    assert.equal(url.username, databaseRole);
    assert.equal(url.password, readFileSync(join(f.root, "secrets", "migrate", role.toUpperCase() + "_DB_PASSWORD"), "utf8").trim());
  }
  const master = join(f.root, "secrets", "db", "POSTGRES_PASSWORD");
  assert.equal(lstatSync(master).uid, 1000);
  assert.equal(lstatSync(master).mode & 0o777, 0o600);
  const privateKey = readFileSync(join(f.root, "secrets", "signing", "key-v1.pem"));
  const publicKey = JSON.parse(readFileSync(join(f.root, "secrets", "public", "key-v1.json"), "utf8")).keys[0];
  assert.equal(createPublicKey(privateKey).export({ format: "jwk" }).x, publicKey.x);
  assert.equal(publicKey.d, undefined);
});

test("a restart keeps credentials and signing bytes unchanged", t => {
  const f = fixture(t);
  f.initialize();
  const path = join(f.root, "secrets", "web", "DATABASE_URL");
  const signing = join(f.root, "secrets", "signing", "key-v1.pem");
  const before = [readFileSync(path), readFileSync(signing)];
  writeFileSync(join(f.data, "PG_VERSION"), "17");
  f.initialize();
  assert.deepEqual([readFileSync(path), readFileSync(signing)], before);
});

test("an existing database without its credential store is left untouched", t => {
  const f = fixture(t);
  writeFileSync(join(f.data, "PG_VERSION"), "17");
  assert.throws(f.initialize, /original protected credential files/);
  assert.equal(existsSync(join(f.root, "secrets", "db")), false);
});

test("retired configuration fails before generating files or echoing values", t => {
  const f = fixture(t);
  for (const name of ["PULS_TOKEN", "PULS_ALLOW_SHARED_TOKEN", "PULS_API_TOKEN", "PULS_USER_ID", "WEB_AUTH_PASSWORD", "WEB_DEMO_USER", "POSTGRES_PASSWORD", "WEB_SES_SECRET_ACCESS_KEY"]) {
    writeFileSync(f.settings, name + "=synthetic-private-value\n");
    assert.throws(() => validateSettings(f.settings), error => error.message.includes(name) && !error.message.includes("synthetic-private-value"));
    assert.throws(f.initialize);
    assert.equal(existsSync(join(f.root, "secrets", "db")), false);
  }
  for (const value of ["WEB_ACCOUNTS=false\n", "WEB_ACCOUNTS=true\nWEB_ACCOUNTS=true\n", "invalid line"]) {
    writeFileSync(f.settings, value);
    assert.throws(f.initialize);
  }
});

test("public or mismatched existing files are preserved and refused", t => {
  const f = fixture(t);
  f.initialize();
  const path = join(f.root, "secrets", "api", "DATABASE_URL");
  chmodSync(path, 0o644);
  assert.throws(f.initialize, /permissions/);
  chmodSync(path, 0o600);
  writeFileSync(path, "postgres://synthetic-mismatch\n");
  assert.throws(f.initialize, /does not match/);
  assert.equal(readFileSync(path, "utf8"), "postgres://synthetic-mismatch\n");
});

test("a symlink credential store is not followed", t => {
  const f = fixture(t);
  const outside = join(f.root, "outside");
  mkdirSync(outside);
  symlinkSync(outside, join(f.root, "secrets", "migrate"));
  assert.throws(f.initialize, /Invalid secret directory/);
});

test("actual application UIDs cannot read another role or migration credentials", t => {
  const f = fixture(t);
  f.initialize();
  const paths = ["api/DATABASE_URL", "ingest/DATABASE_URL", "web/DATABASE_URL", "db/POSTGRES_PASSWORD", "migrate/API_DB_PASSWORD", "signing/key-v1.pem"];
  for (const [uid, readable] of [[65532, [0]], [65533, [1]], [1001, [2, 5]]]) {
    for (const [index, path] of paths.entries()) {
      const read = () => execFileSync(process.execPath, ["-e", "require('node:fs').readFileSync(process.argv[1])", join(f.root, "secrets", path)], { uid, gid: uid, stdio: "ignore" });
      if (readable.includes(index)) assert.doesNotThrow(read);
      else assert.throws(read);
    }
  }
});
