// The single data API the UI talks to, re-exported from lib/data/:
//
//   source.ts       where data comes from (live / demo / error), liveRead()
//   unavailable.ts  DataUnavailableError, what a page throws when it cannot read
//   metricDaily.ts  whether charts may read metric_daily (zone gate, per-user types)
//   series.ts       chart series, latest readings, Today's totals, sparklines
//   stats.ts        rows and date range per type
//   rings.ts        today's activity rings
//   workouts.ts     workouts, one workout with its route, its series streams
//   users.ts        the switcher's users, one user's row, the HR-zone profile
//
// Every health-data function takes the user to read as its first argument;
// pages resolve it once per request with lib/viewer.ts (`viewerUser()`),
// which keeps these modules free of `cookies()` and testable without a request.
//
// Every health-data read runs inside `scoped(userId, …)` (lib/db.ts): one
// read-only transaction with puls.user_id set to that user. In accounts mode
// the viewer connects as web_app and the database filters every health
// relation on that setting, so a query here cannot return another person's
// rows even if its own filter were wrong. The `user_id = $n` filters stay
// anyway: they are what scopes Basic mode (the grafana role), and the
// planner uses them. Inside a `scoped` callback, use its `q` only — see
// `scoped` for why a second connection there is a hazard (queries.test.ts
// checks every function exported here).
//
// When a read cannot be answered live, it returns demo data on the dev-only
// "demo" source and otherwise throws DataUnavailableError, which the page's
// error boundary (app/error.tsx) shows as "Database unavailable" — never an
// empty chart. See lib/data/source.ts.

export { getDataSource } from "./data/source";
export { DataUnavailableError, isDataUnavailable } from "./data/unavailable";
export { categoryAggregation, getDailySparklines, getLatestMany, getSeries, getTodayTotals, type CategoryAggregation } from "./data/series";
export { getStats } from "./data/stats";
export { getActivityRings } from "./data/rings";
export { getWorkoutDetail, getWorkouts, getWorkoutSeries } from "./data/workouts";
export { DEFAULT_MAX_HR, getProfile, getUser, profileFromDob, profileFromStoredValues } from "./data/users";
export { getSleepSessions } from "./data/sleep";
