export type ReadOperation = "metrics:read" | "samples:read";

export interface ReadPermissions {
  operations: ReadOperation[];
  record_types: string[];
  time_range: { from: string; until: string };
}

function object(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function exact(value: Record<string, unknown>, keys: string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

/** UTC instants retain fractional precision when comparing authorization bounds. */
function instant(value: unknown): bigint | null {
  if (typeof value !== "string") return null;
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$/.exec(value);
  if (!match) return null;
  const milliseconds = Date.parse(`${match[1]}Z`);
  if (!Number.isFinite(milliseconds) || new Date(milliseconds).toISOString().slice(0, 19) !== match[1]) return null;
  return BigInt(milliseconds) * 1_000_000n + BigInt((match[2] ?? "").padEnd(9, "0"));
}

export function readPermissions(value: unknown): ReadPermissions | null {
  if (!object(value) || !exact(value, ["operations", "record_types", "time_range"])) return null;
  const operations = value.operations;
  const types = value.record_types;
  const range = value.time_range;
  if (!Array.isArray(operations) || !operations.length || new Set(operations).size !== operations.length ||
      !operations.every((op): op is ReadOperation => op === "metrics:read" || op === "samples:read") ||
      !Array.isArray(types) || !types.length || new Set(types).size !== types.length ||
      !types.every((type): type is string => typeof type === "string" && [...type].length >= 1 && [...type].length <= 256) ||
      !object(range) || !exact(range, ["from", "until"]) || typeof range.from !== "string" || typeof range.until !== "string") return null;
  const from = instant(range.from), until = instant(range.until);
  if (from === null || until === null || from >= until) return null;
  return { operations, record_types: types, time_range: { from: range.from, until: range.until } };
}

export function permissionsFromForm(form: FormData | null): ReadPermissions | null {
  const utc = (key: string) => {
    const values = form?.getAll(key) ?? [];
    if (values.length !== 1 || typeof values[0] !== "string") return null;
    const value = values[0];
    return /^\d{4}-\d\d-\d\dT\d\d:\d\d$/.test(value) ? `${value}:00Z` : `${value}Z`;
  };
  return readPermissions({ operations: form?.getAll("operation"), record_types: form?.getAll("record_type"), time_range: { from: utc("from"), until: utc("until") } });
}

export function permissionsEqual(left: ReadPermissions, right: ReadPermissions): boolean {
  return left.operations.length === right.operations.length && left.operations.every((op) => right.operations.includes(op)) &&
    left.record_types.length === right.record_types.length && left.record_types.every((type) => right.record_types.includes(type)) &&
    instant(left.time_range.from) === instant(right.time_range.from) && instant(left.time_range.until) === instant(right.time_range.until);
}

export function withinPermissionRange(permissions: ReadPermissions, from: string, until: string): boolean {
  const start = instant(from), end = instant(until);
  const allowedStart = instant(permissions.time_range.from), allowedEnd = instant(permissions.time_range.until);
  return start !== null && end !== null && allowedStart !== null && allowedEnd !== null && start < end && start >= allowedStart && end <= allowedEnd;
}
