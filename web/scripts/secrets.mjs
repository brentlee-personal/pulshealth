import { closeSync, constants, fstatSync, openSync, readSync } from "node:fs";
import { isAbsolute } from "node:path";

/** @param {string} name @param {Record<string, string | undefined>} [env] */
export function readSecret(name, env = process.env) {
  if (env[name]) throw new Error(`Use ${name}_FILE; plaintext ${name} is not accepted`);
  const path = env[`${name}_FILE`];
  if (!path) return undefined;
  let descriptor;
  try {
    if (!isAbsolute(path)) throw new Error();
    descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const info = fstatSync(descriptor);
    if (!info.isFile() || (info.mode & 0o077) !== 0 || (info.mode & 0o400) === 0 || info.size < 1 || info.size > 8192) throw new Error();
    const bytes = Buffer.alloc(8193);
    const size = readSync(descriptor, bytes, 0, bytes.length, 0);
    if (size !== info.size || size > 8192) throw new Error();
    const value = new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, size)).replace(/\r?\n$/, "");
    if (!value || value.includes("\r") || value.includes("\n") || value.includes("\0")) throw new Error();
    return value;
  } catch {
    throw new Error(`${name}_FILE is unavailable or is not a private secret file`);
  } finally {
    if (descriptor !== undefined) closeSync(descriptor);
  }
}
