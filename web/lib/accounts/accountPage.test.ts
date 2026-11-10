import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/reportingTimeZone", () => ({ reportingTimeZone: async () => "Pacific/Auckland" }));
const viewer = vi.hoisted(() => ({ currentSession: vi.fn() }));
const client = vi.hoisted(() => ({ userAgent: "" }));
vi.mock("next/headers", () => ({ headers: async () => new Headers({ "user-agent": client.userAgent }) }));
const lookups = vi.hoisted(() => ({ listSessions: vi.fn(), myDevices: vi.fn(), listConnectedApps: vi.fn() }));
vi.mock("@/lib/viewer", () => viewer);
vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw new Error(`redirect:${to}`);
  },
  notFound: () => {
    throw new Error("notFound");
  },
}));
vi.mock("@/lib/accounts/session", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./session")>()),
  listSessions: lookups.listSessions,
}));
vi.mock("@/lib/accounts/signups", async (importOriginal) => ({ ...(await importOriginal<typeof import("./signups")>()), myDevices: lookups.myDevices }));
vi.mock("@/lib/oauth/store", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../oauth/store")>()),
  listConnectedApps: lookups.listConnectedApps,
}));

const PERSON = {
  id: Buffer.alloc(32, 3),
  accountId: "00000000-0000-4000-8000-000000000001",
  userId: "0d3a0000-0000-4000-8000-0000000000ab",
  email: "person@example.invalid",
  isAdmin: false,
  selfService: false,
  refreshed: false,
};

const saved = { ...process.env };
beforeAll(() => {
  process.env.WEB_ACCOUNTS = "true";
});
afterAll(() => {
  process.env = saved;
});
beforeEach(() => {
  viewer.currentSession.mockReset().mockResolvedValue(PERSON);
  for (const fn of Object.values(lookups)) fn.mockReset();
  delete process.env.WEB_SIGNUPS;
  client.userAgent = "";
});

async function render(query: Record<string, string> = {}): Promise<string> {
  const { default: AccountPage } = await import("@/app/account/page");
  const element = await AccountPage({ searchParams: Promise.resolve(query) });
  return renderToStaticMarkup(createElement(() => element));
}

describe("personal phone pairing", () => {
  it.each([false, true])("offers pairing regardless of signup origin (selfService=%s)", async (selfService) => {
    viewer.currentSession.mockResolvedValue({ ...PERSON, selfService, canDelete: selfService });
    lookups.listSessions.mockResolvedValue([]);
    lookups.myDevices.mockResolvedValue([]);
    lookups.listConnectedApps.mockResolvedValue([]);
    const html = await render();
    expect(html).toContain("Connect this iPhone");
    expect(lookups.myDevices).toHaveBeenCalledWith(PERSON.id);
    // Granting pairing must not expose operator-managed account deletion.
    expect(html.includes('action="/api/auth/delete-account"')).toBe(selfService);
  });

  it("opens a focused connection page for an existing personal session without minting on GET", async () => {
    viewer.currentSession.mockResolvedValue({ ...PERSON });
    const { default: ConnectPage } = await import("@/app/connect/iphone/page");
    const element = await ConnectPage({ searchParams: Promise.resolve({}) });
    const html = renderToStaticMarkup(createElement(() => element));
    expect(html).toContain('action="/api/auth/connect-iphone"');
    expect(html).toContain("Save &amp; Apply before Health data is uploaded");
    expect(html).toContain('name="next" value="/connect/iphone"');
    for (const fn of Object.values(lookups)) expect(fn).not.toHaveBeenCalled();
  });

  it("requires sign-in before showing the phone connection form", async () => {
    viewer.currentSession.mockResolvedValue(null);
    const { default: ConnectPage } = await import("@/app/connect/iphone/page");
    await expect(ConnectPage({ searchParams: Promise.resolve({}) })).rejects.toThrow(/redirect:.*login/);
    for (const fn of Object.values(lookups)) expect(fn).not.toHaveBeenCalled();
  });});


it("returns directly to the app from the account page on an iPhone, for older app versions", async () => {
  client.userAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_5 like Mac OS X) Safari/605.1.15";
  viewer.currentSession.mockResolvedValue({ ...PERSON, selfService: false });
  lookups.listSessions.mockResolvedValue([]);
  lookups.myDevices.mockResolvedValue([]);
  lookups.listConnectedApps.mockResolvedValue([]);
  const html = await render();
  expect(html).toContain('action="/api/auth/connect-iphone"');
  expect(html).toContain("return to PulsHealth");
});
