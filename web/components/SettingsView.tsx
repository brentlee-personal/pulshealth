"use client";

// Settings: the view-only metric/imperial toggle (persisted in localStorage) and
// a read-only summary of the user being shown, the synced profile and the
// heart-rate zones it implies.

import { useUnits } from "./UnitsProvider";
import { UNIT_OPTIONS, defaultUnits, type UnitCategory, type UnitSystem, type UnitOverrides } from "@/lib/units";
import { displayUnit } from "@/lib/format";
import { hrZones, type HrZone } from "@/lib/zones";
import type { Profile, User } from "@/lib/types";

export function SettingsView({ profile, user }: { profile: Profile; user: User }) {
  const { system, setSystem, preferences, setOverrides } = useUnits();
  const zones = hrZones(profile.maxHr, profile.restingHr);

  return (
    <>
      <section style={{ marginTop: 8 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Display units</div>
        <div className="panel" style={{ padding: "18px 20px", display: "flex", justifyContent: "space-between", alignItems: "center", gap: 16, flexWrap: "wrap" }}>
          <div>
            <div style={{ fontSize: 14, fontWeight: 550 }}>Measurement system</div>
            <div style={{ fontSize: 12.5, color: "var(--muted)", marginTop: 3 }}>
              Applies across the dashboard, metrics, charts, and workouts. Choosing a system resets the choices below. Saved in this browser.
            </div>
          </div>
          <div style={{ display: "inline-flex", background: "var(--border)", borderRadius: 9, padding: 3 }}>
            {(["metric", "imperial"] as UnitSystem[]).map((s) => (
              <button
                key={s}
                type="button"
                aria-pressed={system === s}
                onClick={() => setSystem(s)}
                style={{
                  cursor: "pointer",
                  border: "none",
                  borderRadius: 7,
                  padding: "7px 16px",
                  fontSize: 13,
                  fontWeight: 550,
                  textTransform: "capitalize",
                  background: system === s ? "var(--panel)" : "transparent",
                  color: system === s ? "var(--fg)" : "var(--muted)",
                  boxShadow: system === s ? "0 1px 3px rgba(0,0,0,0.25)" : "none",
                }}
              >
                {s}
              </button>
            ))}
          </div>
        </div>
        <div className="panel" style={{ marginTop: 12, padding: "4px 20px" }}>
          {Object.entries(UNIT_OPTIONS).map(([key, config]) => {
            const category = key as UnitCategory;
            return <label key={key} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 16, padding: "12px 0" }}>
              <span style={{ fontSize: 14 }}>{config.label}</span>
              <select aria-label={config.label} value={preferences[category] ?? defaultUnits(system)[category]}
                onChange={(event) => { const { system: _system, ...overrides } = preferences; void _system; setOverrides({ ...overrides, [key]: event.target.value } as UnitOverrides); }}
                style={{ background: "var(--panel)", color: "var(--fg)", border: "1px solid var(--border)", borderRadius: 7, padding: "7px 12px" }}>
                {config.options.map((unit) => <option key={unit} value={unit}>{unit === "fl oz" ? "US fl oz" : displayUnit(unit)}</option>)}
              </select>
            </label>;
          })}
        </div>
        <p style={{ fontSize: 12.5, color: "var(--muted)" }}>Distance, speed, pace, elevation, and gait lengths follow the measurement system. Nutrition uses grams and milligrams; heart rate, durations, percentages, and other specialized readings keep their standard units.</p>
      </section>

      <section style={{ marginTop: 24 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Profile</div>
        <div className="panel" style={{ overflow: "hidden" }}>
          <Row label="User" value={user.name?.trim() || user.email?.trim() || `User ${user.id.slice(0, 8)}`} first />
          <Row label="User ID" value={user.id} mono />
          <Row label="Age" value={profile.age != null ? `${profile.age}` : "Not synced"} />
          <Row label="Biological sex" value={profile.biologicalSex ? cap(profile.biologicalSex) : "Not synced"} />
          <Row label="Max heart rate" value={`${profile.maxHr} bpm${profile.age == null ? " (default)" : ""}`} />
          <Row label="Resting heart rate" value={profile.restingHr != null ? `${Math.round(profile.restingHr)} bpm` : "Not synced"} />
        </div>
        <div style={{ fontSize: 12, color: "var(--faint)", marginTop: 8 }}>
          Heart-rate zones use Heart Rate Reserve: resting HR + a percentage of max HR minus resting HR.
        </div>
      </section>

      <section style={{ marginTop: 24 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Heart-rate zones</div>
        <div className="panel" style={{ overflow: "hidden" }}>
          {zones.slice().reverse().map((z, i) => (
            <div key={z.index} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 16, padding: "12px 18px", borderTop: i === 0 ? "none" : "1px solid var(--border)", fontSize: 13.5 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 9 }}>
                <span style={{ width: 9, height: 9, borderRadius: 3, background: z.color }} />
                <span style={{ fontWeight: 550 }}>Zone {z.index}</span>
                {z.loPct != null && <span style={{ color: "var(--faint)", fontSize: 12 }}>{z.loPct}%+</span>}
              </div>
              <span className="mono" style={{ color: "var(--fg-soft)" }}>
                {formatZoneRange(z)}
              </span>
            </div>
          ))}
        </div>
      </section>
    </>
  );
}

function formatZoneRange(zone: HrZone): string {
  if (zone.loBpm == null && zone.hiBpm != null) return `<${zone.hiBpm} bpm`;
  if (zone.hiBpm == null && zone.loBpm != null) return `${zone.loBpm}+ bpm`;
  if (zone.loBpm != null && zone.hiBpm != null) return `${zone.loBpm}–${zone.hiBpm} bpm`;
  return "All bpm";
}

function cap(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

function Row({ label, value, first, mono }: { label: string; value: string; first?: boolean; mono?: boolean }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", gap: 16, padding: "12px 18px", borderTop: first ? "none" : "1px solid var(--border)", fontSize: 13.5 }}>
      <span style={{ color: "var(--muted)" }}>{label}</span>
      <span className={mono ? "mono" : undefined} style={{ color: "var(--fg-soft)", fontSize: mono ? 12.5 : undefined, overflowWrap: "anywhere", textAlign: "right" }}>{value}</span>
    </div>
  );
}
