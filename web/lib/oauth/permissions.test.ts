import { describe, expect, it } from "vitest";
import { defaultReadWindow, permissionsFromForm, readPermissions, readRequestAllowed, type ReadPermissions } from "./permissions";

const permissions = {
  operations: ["metrics:read"], record_types: ["HKQuantityTypeIdentifierStepCount"],
  time_range: { from: "2026-01-01T00:00:00Z", until: "2026-02-01T00:00:00Z" },
} satisfies ReadPermissions;

describe("personal read permissions", () => {
  it("keeps explicit operations, generic type names, and nanosecond UTC bounds", () => {
    expect(readPermissions(permissions)).toEqual(permissions);
    expect(readPermissions({ ...permissions, record_types: ["oura.sleep", "测".repeat(256)] })).not.toBeNull();
    expect(readPermissions({ ...permissions, time_range: { from: "2026-01-01T00:00:00.000000001Z", until: "2026-01-01T00:00:00.000000002Z" } })).not.toBeNull();
  });

  it("rejects missing, extra, duplicated, writable, and malformed scopes", () => {
    for (const value of [null, [], {}, { ...permissions, member_id: "another member" },
      { ...permissions, operations: [] }, { ...permissions, operations: ["metrics:read", "metrics:read"] },
      { ...permissions, operations: ["data:delete"] }, { ...permissions, operations: [null] },
      { ...permissions, record_types: [] }, { ...permissions, record_types: ["same", "same"] },
      { ...permissions, record_types: [null] }, { ...permissions, record_types: [""] },
      { ...permissions, record_types: ["测".repeat(257)] },
      ...["2026-02-30T00:00:00Z", "2026-01-01T24:00:00Z", "2026-01-01T00:00:00+00:00", "2026-01-01", "bad"].map((from) => ({ ...permissions, time_range: { ...permissions.time_range, from } })),
      { ...permissions, time_range: { from: permissions.time_range.until, until: permissions.time_range.from } },
      { ...permissions, time_range: { ...permissions.time_range, extra: true } },
    ]) expect(readPermissions(value), JSON.stringify(value)).toBeNull();
  });

  it("requires an explicit valid form and never adds raw access", () => {
    expect(permissionsFromForm(null)).toBeNull();
    const form = new FormData();
    form.set("operation", "metrics:read"); form.set("record_type", permissions.record_types[0]);
    form.set("from", "2026-01-01T00:00"); form.set("until", "2026-02-01T00:00:00");
    expect(permissionsFromForm(form)).toEqual(permissions);
    form.append("operation", "samples:read");
    expect(permissionsFromForm(form)?.operations).toEqual(["metrics:read", "samples:read"]);
    form.append("until", "2030-01-01T00:00:00");
    expect(permissionsFromForm(form)).toBeNull();
  });

  it("rejects reversed or equal fractional bounds without rounding", () => {
    expect(readPermissions({ ...permissions, time_range: { from: "2026-01-01T00:00:00.000000002Z", until: "2026-01-01T00:00:00.000000001Z" } })).toBeNull();
    expect(readPermissions({ ...permissions, time_range: { from: "2026-01-01T00:00:00Z", until: "2026-01-01T00:00:00.000000000Z" } })).toBeNull();
  });
  it("requires every requested operation, type and exact time bound", () => {
    const start = Date.UTC(2026, 0, 1), end = Date.UTC(2026, 1, 1);
    expect(readRequestAllowed(permissions, "metrics:read", permissions.record_types, start, end)).toBe(true);
    expect(readRequestAllowed(permissions, "samples:read", permissions.record_types, start, end)).toBe(false);
    expect(readRequestAllowed(permissions, "metrics:read", [], start, end)).toBe(false);
    expect(readRequestAllowed(permissions, "metrics:read", ["oura.sleep"], start, end)).toBe(false);
    expect(readRequestAllowed(permissions, "metrics:read", permissions.record_types, start - 1, end)).toBe(false);
    expect(readRequestAllowed(permissions, "metrics:read", permissions.record_types, start, end + 1)).toBe(false);
    expect(readRequestAllowed(permissions, "metrics:read", permissions.record_types, NaN, end)).toBe(false);
  });
  it("rounds default millisecond bounds inward and never extends a historical grant to now", () => {
    const from = Date.UTC(2026, 0, 1), until = Date.UTC(2026, 0, 2);
    const fractional = { ...permissions, time_range: { from: "2026-01-01T00:00:00.000000001Z", until: "2026-01-02T00:00:00.000000001Z" } };
    expect(defaultReadWindow(fractional, Date.UTC(2026, 0, 3))).toEqual({ start: from + 1, end: until });
    expect(defaultReadWindow(fractional, Date.UTC(2025, 0, 1))).toBeNull();
  });
});
