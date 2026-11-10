// Who may use /admin and the account page's self-service. The database
// functions behind them take `Session.id` (the stored hash) and check it
// themselves. Server-only.

import { viewerMode } from "../mode";
import { currentSession } from "../viewer";
import type { Session } from "./session";

/** The signed-in administrator, or null. */
export async function currentAdmin(): Promise<Session | null> {
  if (viewerMode() !== "accounts") return null;
  const session = await currentSession().catch(() => null);
  return session?.isAdmin ? session : null;
}

/** The signed-in person (any account), or null. */
export async function currentAccount(): Promise<Session | null> {
  if (viewerMode() !== "accounts") return null;
  return currentSession().catch(() => null);
}
