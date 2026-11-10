// Personal daily views compute each account's calendar in the database.
import { reportingTimeZone } from "../reportingTimeZone";

export const DAY_MS = 86_400_000;

export async function metricDailyUsable(userId: string): Promise<boolean> {
  await reportingTimeZone(userId);
  return true;
}
