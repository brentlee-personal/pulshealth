#!/usr/bin/env node
import { randomBytes } from "node:crypto";
import { chownSync, chmodSync, closeSync, existsSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { createKeyFiles } from "./auth-keys.mjs";
import { readSecret } from "./secrets.mjs";

const retired = new Set([
  "DATABASE_URL", "POSTGRES_PASSWORD", "PGPASSWORD", "GRAFANA_DB_PASSWORD",
  "API_DB_PASSWORD", "INGEST_DB_PASSWORD", "WEB_DB_PASSWORD", "WEB_DATABASE_URL",
  "PULS_TOKEN", "PULS_ALLOW_SHARED_TOKEN", "PULS_API_TOKEN", "PULS_MULTI_USER",
  "PULS_USER_ID", "WEB_AUTH_PASSWORD", "WEB_DEMO_USER", "INGEST_DB_USER",
  "PULS_MCP_OAUTH_SECRET", "WEB_SES_ACCESS_KEY_ID", "WEB_SES_SECRET_ACCESS_KEY",
]);

export function validateSettings(path) {
  const info = lstatSync(path);
  if (!info.isFile() || info.isSymbolicLink() || info.size > 65536) throw new Error("Invalid settings file");
  const seen = new Set();
  for (const line of readFileSync(path, "utf8").split(/\r?\n/)) {
    if (!line.trim() || line.trimStart().startsWith("#")) continue;
    const match = /^([A-Z][A-Z0-9_]*)=(.*)$/.exec(line);
    if (!match || seen.has(match[1])) throw new Error("Settings must contain unique NAME=value lines");
    const [, name, raw] = match;
    seen.add(name);
    const value = raw.trim().replace(/^(['"])(.*)\1$/, "$2");
    if (retired.has(name) && value !== "") throw new Error(`Retired setting: ${name}`);
    if (name === "WEB_ACCOUNTS" && value !== "" && value !== "true") throw new Error("Personal accounts cannot be disabled");
  }
}

function directory(path, uid, mode = 0o700) {
  mkdirSync(path, { recursive: true, mode });
  const info = lstatSync(path);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("Invalid secret directory");
  if (readdirSync(path).length === 0) {
    chmodSync(path, mode);
    chownSync(path, uid, uid);
  } else if ((info.mode & 0o777) !== mode || info.uid !== uid) {
    throw new Error("Existing secret directory permissions must be restored");
  }
}

function owner(path, uid, mode = 0o600) {
  const info = lstatSync(path);
  if (!info.isFile() || info.isSymbolicLink() || info.uid !== uid || (info.mode & 0o777) !== mode) {
    throw new Error("Existing secret file permissions must be restored");
  }
}

function put(path, value, uid) {
  if (existsSync(path)) {
    owner(path, uid);
    if (readSecret("VALUE", { VALUE_FILE: path }) !== value) throw new Error("Existing application credential does not match its role");
    return;
  }
  const descriptor = openSync(path, "wx", 0o600);
  try {
    writeFileSync(descriptor, value + "\n");
    fsyncSync(descriptor);
    chownSync(path, uid, uid);
  } finally { closeSync(descriptor); }
}

export function initializeSecrets(root, dataDirectory, settings) {
  if (process.getuid?.() !== 0) throw new Error("Secret initialization must run in its root-only container");
  validateSettings(settings);
  const existingDatabase = existsSync(join(dataDirectory, "PG_VERSION"));
  const migrate = join(root, "migrate");
  if (existingDatabase && !existsSync(join(root, "db", "POSTGRES_PASSWORD"))) {
    throw new Error("Existing database requires its original protected credential files; nothing was generated");
  }
  for (const [name, uid] of [["db", 1000], ["migrate", 1000], ["api", 65532], ["ingest", 65533], ["web", 1001], ["signing", 1001]]) {
    directory(join(root, name), uid);
  }
  directory(join(root, "public"), 1001, 0o755);
  const values = {};
  for (const name of ["POSTGRES_PASSWORD", "GRAFANA_DB_PASSWORD", "API_DB_PASSWORD", "INGEST_DB_PASSWORD", "WEB_DB_PASSWORD"]) {
    const path = join(name === "POSTGRES_PASSWORD" ? join(root, "db") : migrate, name);
    if (existsSync(path)) {
      owner(path, 1000);
      values[name] = readSecret(name, { [name + "_FILE"]: path });
      if (!/^[a-f0-9]{64}$/.test(values[name])) throw new Error("Existing role credential is invalid");
    } else {
      if (existingDatabase) throw new Error("Existing database requires all original protected role files");
      values[name] = randomBytes(32).toString("hex");
      put(path, values[name], 1000);
    }
  }
  for (const [role, uid] of [["api", 65532], ["ingest", 65533], ["web", 1001]]) {
    const databaseRole = role === "api" ? "api_reader" : role === "web" ? "web_app" : "ingest";
    const value = `postgres://${databaseRole}:${values[role.toUpperCase() + "_DB_PASSWORD"]}@db:5432/postgres?sslmode=disable`;
    put(join(root, role, "DATABASE_URL"), value, uid);
  }
  const privateFile = join(root, "signing", "key-v1.pem");
  const publicFile = join(root, "public", "key-v1.json");
  if (!existsSync(privateFile) && !existsSync(publicFile)) {
    if (existingDatabase) throw new Error("Existing database requires its original signing files");
    createKeyFiles(join(root, "signing"), join(root, "public"), "key-v1");
    chownSync(privateFile, 1001, 1001);
    chownSync(publicFile, 1001, 1001);
  }
  owner(privateFile, 1001);
  owner(publicFile, 1001, 0o644);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [root, dataDirectory, settings, extra] = process.argv.slice(2);
  try {
    if (!root || !dataDirectory || !settings || extra) throw new Error("usage: init-secrets.mjs <secret-root> <database-directory> <settings-file>");
    initializeSecrets(root, dataDirectory, settings);
    console.log("Protected application files are ready; existing values were preserved");
  } catch (error) {
    console.error(error instanceof Error ? error.message : "Secret initialization failed");
    process.exitCode = 1;
  }
}
