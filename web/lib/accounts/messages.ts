// What the accounts pages say for each `?error=` / `?notice=` code the
// handlers redirect with. Codes, not prose, travel in the URL, so a link
// cannot be made to show arbitrary text.

import { PASSWORD_MAX_LENGTH, PASSWORD_MIN_LENGTH } from "./password";

const ERRORS: Record<string, string> = {
  time_zone: "Choose a valid IANA time zone, such as America/Los_Angeles.",
  invalid: "That email and password do not match an account.",
  throttled: "Too many attempts. Wait a minute, then try again.",
  unavailable: "Something went wrong on the server. Try again in a moment.",
  invite: "That invite link is not valid. Ask for a new one.",
  mismatch: "The two passwords do not match.",
  short: `Use at least ${PASSWORD_MIN_LENGTH} characters.`,
  long: `Use at most ${PASSWORD_MAX_LENGTH} characters.`,
  current: "Your current password was not right.",
  email_taken: "Another account already uses this email address. Ask for a new invite.",
  email: "Enter a valid email address.",
  consent: "Tick the box to say you have read the privacy policy.",
  forbidden: "Only an administrator can do that.",
  exists: "An account already signs in with that address.",
  failed: "That did not work. Try again, and check the server log if it keeps failing.",
  devices: "You have ten connected iPhones already. Disconnect one first.",
  deletion_requested: "That person asked to be deleted, so their account stays disabled. Purge their data instead.",
  purge_running: "A purge of that user is already running. It disappears from this list when it is done.",
  own_account: "You cannot disable or enable your own account here.",
  admin_account: "Administrators' accounts are managed from the server, not here.",
  none_selected: "No one was ticked. Tick the ones to decline first.",
};

const NOTICES: Record<string, string> = {
  time_zone: "Reporting time zone saved.",
  "signed-out": "You are signed out.",
  recovered: "Password reset. Sign in with your new password. All previous browser sessions have been signed out.",
  password: "Password changed. Every other browser signed in to this account has been signed out.",
  sessions: "Signed out.",
  received: "Thanks for signing up! PulsHealth is currently at capacity, so you've joined the waitlist. We'll email you when access is available.",
  denied: "Declined and removed from the waitlist. No email was sent.",
  denied_many: "Those were declined and removed from the waitlist. No email was sent.",
  disabled: "Account disabled: it cannot sign in, and its iPhones were disconnected.",
  enabled: "Account enabled again. Its iPhones stay disconnected until reconnected.",
  purged: "Everything stored for that user was deleted.",
  device_revoked: "That iPhone is disconnected and can no longer upload.",
  assistant_revoked: "Revoked. The assistant can no longer refresh its access, and what it holds expires within 30 minutes.",
  deleted: "Your account and health records have been removed from the live database.",
};


/** One query parameter as a single string (Next.js hands over string | string[]). */
export function param(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function errorMessage(code: string | undefined, overrides: Record<string, string> = {}): string | null {
  if (!code) return null;
  return overrides[code] ?? ERRORS[code] ?? null;
}

export function noticeMessage(code: string | undefined): string | null {
  return code ? (NOTICES[code] ?? null) : null;
}
