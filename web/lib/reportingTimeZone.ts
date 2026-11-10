// Server-only reporting calendar. Never cache one account's zone globally.
import { scoped } from "./db";
import { viewerMode } from "./mode";

export async function reportingTimeZone(userId: string): Promise<string> {
  viewerMode();
  const rows = await scoped(userId, (q) => q<{ zone: string }>(
    "SELECT puls_user_time_zone($1::uuid) AS zone", [userId],
  ));
  const zone = rows[0]?.zone;
  if (!zone) throw new Error("Reporting time zone unavailable");
  new Intl.DateTimeFormat("en", { timeZone: zone }).format();
  return zone;
}
