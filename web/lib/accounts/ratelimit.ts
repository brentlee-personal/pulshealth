// Failed-sign-in throttling for accounts mode.
//
// The same policy as server/ingest/ratelimit.go and server/api/ratelimit.go
// (copied, not shared, as those two are): a token bucket per key that ONLY
// failures draw from — a wrong password, an unknown or expired invite link,
// a wrong current password on the account page. A successful sign-in never
// costs a token. Once a bucket is empty the attempt is refused BEFORE the
// password is checked, so an attacker who is out of tokens learns nothing
// from the answer. Keep the constants in step with the Go copies.
//
// One difference from the Go copies, which compare a token in microseconds:
// a password check here takes tens of milliseconds of scrypt and a database
// round trip. Charging only after the check let a burst of parallel guesses
// all pass the check before the first was charged — at scrypt speed, not ten
// a minute. So an attempt takes its token up front, in the same synchronous
// step as the check (`takeAll`), and gets it back when the attempt turns out
// not to be a failure (`refundAll`): the net effect is still "failures only".
//
// Two kinds of key are charged per failure: the client address and, for a
// sign-in, the email address tried. The address bucket stops one client
// spraying many accounts; the email bucket stops many clients (or a forged
// address header) guessing at one account.
//
// The email bucket alone would let anyone lock a person out: ten wrong
// guesses a minute, from anywhere, keep it empty for as long as they care
// to. So a sign-in from an address the account has proved — one of its own
// live sessions signed in from it (`signedInFrom` in session.ts) — is charged
// to its address bucket only (`failureKeys(ip, email, true)`): the owner at
// home or on their usual network gets in while strangers hammer the email.
// Considered and not chosen: refusing only when BOTH buckets are empty lets a
// botnet of fresh addresses guess at one account without limit; a larger,
// slower email bucket does not end the lockout — any finite refill can be
// held empty by a sender at that rate — it only changes the price. What the
// exemption costs: an attacker who shares a known address (the same NAT)
// skips the email bucket, but still has its address bucket, so the guessing
// rate at one account stays bounded — ten a minute, plus ten per address the
// account has live sessions from. The owner on a new address is still
// subject to the email bucket; the hosted instance also sits behind
// Cloudflare's own rate limits.
//
// In-process memory, so it resets on restart and is per container: the
// viewer runs as one.

export const AUTH_FAILURE_BURST = 10;
export const AUTH_FAILURES_PER_MINUTE = 10;
const IDLE_TTL_MS = 10 * 60_000;
const SWEEP_EVERY_MS = 60_000;
const MAX_KEYS = 10_000;

interface Bucket {
  tokens: number;
  lastSeen: number;
}

export class FailureLimiter {
  private buckets = new Map<string, Bucket>();
  private lastSweep = 0;
  private readonly refillPerMs: number;

  constructor(
    private readonly burst = AUTH_FAILURE_BURST,
    perMinute = AUTH_FAILURES_PER_MINUTE,
    private readonly idleTtlMs = IDLE_TTL_MS,
    private readonly maxKeys = MAX_KEYS,
  ) {
    this.refillPerMs = perMinute / 60_000;
  }

  private refill(bucket: Bucket, now: number): void {
    const elapsed = now - bucket.lastSeen;
    if (elapsed > 0) bucket.tokens = Math.min(this.burst, bucket.tokens + elapsed * this.refillPerMs);
    bucket.lastSeen = now;
  }

  /**
   * Whether an attempt may be evaluated at all; when not, how many seconds
   * until it may (rounded up, so obeying it is never refused again). Unknown
   * keys are allowed and not recorded: only failures create entries.
   */
  check(key: string, now = Date.now()): { allowed: true } | { allowed: false; retryAfterSeconds: number } {
    const bucket = this.buckets.get(key);
    if (!bucket) return { allowed: true };
    this.refill(bucket, now);
    if (bucket.tokens >= 1) return { allowed: true };
    const waitMs = (1 - bucket.tokens) / this.refillPerMs;
    return { allowed: false, retryAfterSeconds: Math.ceil(waitMs / 1000) + 1 };
  }

  /** Gives back a token taken for an attempt that did not fail. */
  refund(key: string, now = Date.now()): void {
    const bucket = this.buckets.get(key);
    if (!bucket) return;
    this.refill(bucket, now);
    bucket.tokens = Math.min(this.burst, bucket.tokens + 1);
  }

  /** Charges one token to `key`. A new bucket starts full. */
  fail(key: string, now = Date.now()): void {
    let bucket = this.buckets.get(key);
    if (!bucket) {
      this.sweep(now);
      bucket = { tokens: this.burst, lastSeen: now };
      this.buckets.set(key, bucket);
    } else {
      this.refill(bucket, now);
    }
    bucket.tokens = Math.max(0, bucket.tokens - 1);
  }

  /** Tracked keys (tests and diagnostics). */
  get size(): number {
    return this.buckets.size;
  }

  // Forget refilled, idle buckets; past the cap, drop the least recently seen
  // quarter so this does not run on every failure.
  private sweep(now: number): void {
    if (now - this.lastSweep < SWEEP_EVERY_MS && this.buckets.size < this.maxKeys) return;
    this.lastSweep = now;
    for (const [key, bucket] of this.buckets) {
      const refilled = bucket.tokens + (now - bucket.lastSeen) * this.refillPerMs;
      if (refilled >= this.burst && now - bucket.lastSeen > this.idleTtlMs) this.buckets.delete(key);
    }
    if (this.buckets.size < this.maxKeys) return;
    const oldestFirst = [...this.buckets.entries()].sort((a, b) => a[1].lastSeen - b[1].lastSeen);
    for (const [key] of oldestFirst.slice(0, this.buckets.size - Math.floor((this.maxKeys * 3) / 4))) {
      this.buckets.delete(key);
    }
  }
}

// One per server process. Kept on globalThis because Next.js may load this
// module more than once (the proxy and the app are bundled separately), and
// every copy must charge the same buckets.
const globalForLimiter = globalThis as typeof globalThis & { __pulsAuthFailures?: FailureLimiter };

/** The limiter every accounts route shares. */
export const authFailures: FailureLimiter = (globalForLimiter.__pulsAuthFailures ??= new FailureLimiter());

/**
 * The bucket keys one attempt is charged to: the client address, and the
 * email tried unless the address is one the account has proved
 * (`knownAddress`, see the top of this file).
 */
export function failureKeys(clientIp: string, email?: string, knownAddress = false): string[] {
  return email && !knownAddress ? [`ip:${clientIp}`, `email:${email}`] : [`ip:${clientIp}`];
}

/** Checks every key; the longest wait wins when any is exhausted. */
export function checkAll(
  limiter: FailureLimiter,
  keys: string[],
  now = Date.now(),
): { allowed: true } | { allowed: false; retryAfterSeconds: number } {
  let wait = 0;
  for (const key of keys) {
    const result = limiter.check(key, now);
    if (!result.allowed) wait = Math.max(wait, result.retryAfterSeconds);
  }
  return wait > 0 ? { allowed: false, retryAfterSeconds: wait } : { allowed: true };
}

export function failAll(limiter: FailureLimiter, keys: string[], now = Date.now()): void {
  for (const key of keys) limiter.fail(key, now);
}

/**
 * Checks every key and, when all allow, takes a token from each — with no
 * await in between, so of any number of concurrent attempts only as many as
 * the buckets hold get through to the password check. Keep the token for a
 * failure; give it back with `refundAll` for anything else.
 */
export function takeAll(
  limiter: FailureLimiter,
  keys: string[],
  now = Date.now(),
): { allowed: true } | { allowed: false; retryAfterSeconds: number } {
  const gate = checkAll(limiter, keys, now);
  if (gate.allowed) failAll(limiter, keys, now);
  return gate;
}

export function refundAll(limiter: FailureLimiter, keys: string[], now = Date.now()): void {
  for (const key of keys) limiter.refund(key, now);
}

// Sign-up requests: three an hour per client address, every request charged
// (asking is not a failure, but nothing legitimate asks more often). Kept on
// globalThis for the same reason as authFailures.
const globalForSignups = globalThis as typeof globalThis & { __pulsSignups?: FailureLimiter };
export const signupRequests: FailureLimiter = (globalForSignups.__pulsSignups ??= new FailureLimiter(3, 0.05));
