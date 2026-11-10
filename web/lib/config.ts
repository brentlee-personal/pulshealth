// Server-side reporting calendar.

// Must match the server stack's PULS_TIME_ZONE (and the phone's zone): the
// database's daily views bucket days in that zone. The stack defaults to UTC.
export const DEFAULT_TIME_ZONE = "UTC";

export function configuredTimeZone(): string {
  const value = process.env.PULS_TIME_ZONE || DEFAULT_TIME_ZONE;
  try {
    new Intl.DateTimeFormat("en", { timeZone: value }).format();
    return value;
  } catch {
    throw new Error(`PULS_TIME_ZONE is not a valid IANA time zone: ${value}`);
  }
}
