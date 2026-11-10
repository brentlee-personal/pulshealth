import { NextRequest } from "next/server";
import { afterAll, beforeEach, expect, it, vi } from "vitest";

const calls = vi.hoisted(() => ({ after: vi.fn(), request: vi.fn(), complete: vi.fn(), session: vi.fn() }));
vi.mock("next/server", async (original) => ({ ...await original<typeof import("next/server")>(), after: calls.after }));
vi.mock("./deletion", () => ({ requestDeletion: calls.request, completeDeletion: calls.complete }));
vi.mock("./session", async (original) => ({ ...await original<typeof import("./session")>(), findSession: calls.session }));
const saved = process.env.WEB_ACCOUNTS;
beforeEach(() => {
  process.env.WEB_ACCOUNTS = "true";
  for (const call of Object.values(calls)) call.mockReset();
  calls.session.mockResolvedValue({ id: Buffer.alloc(32), canDelete: true, isAdmin: false });
  calls.request.mockResolvedValue({ userId: "10000000-0000-4000-8000-000000000001", receipt: "r".repeat(43) });
});
afterAll(() => {
  if (saved === undefined) delete process.env.WEB_ACCOUNTS;
  else process.env.WEB_ACCOUNTS = saved;
});

it("delivers the receipt before starting a potentially long purge", async () => {
  const { POST } = await import("@/app/api/auth/delete-account/route");
  const body = new FormData();
  body.set("confirm", "yes");
  const response = await POST(new NextRequest("https://viewer.example/api/auth/delete-account", { method: "POST", body }));
  expect(response.headers.get("location")).toBe(`/deletion/${"r".repeat(43)}`);
  expect(response.headers.get("set-cookie")).toContain("Max-Age=0");
  expect(calls.complete).not.toHaveBeenCalled();
  expect(calls.after).toHaveBeenCalledTimes(1);
  await calls.after.mock.calls[0][0]();
  expect(calls.complete).toHaveBeenCalledWith("10000000-0000-4000-8000-000000000001");
});
