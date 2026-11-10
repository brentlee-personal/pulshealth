"use client";

import type { RefObject } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { GROUPS, GROUP_LABELS } from "@/lib/catalog";
import { GROUP_COLOR } from "@/lib/colors";
import type { DataSourceInfo } from "@/lib/types";
import { GridIcon, GroupIcon, HomeIcon, SettingsIcon, SignOutIcon, UserIcon, WorkoutIcon } from "./Icons";

export function Sidebar({
  source,
  account,
  menuOpen,
  sidebarRef,
  closeMenu,
}: {
  source: DataSourceInfo;
  account: { email: string; isAdmin: boolean };
  menuOpen: boolean;
  sidebarRef: RefObject<HTMLElement | null>;
  closeMenu: () => void;
}) {
  const path = usePathname();

  const primary = [
    { href: "/", label: "Today", icon: <HomeIcon className="nav-icon" /> },
    { href: "/workouts", label: "Workouts", icon: <WorkoutIcon className="nav-icon" /> },
    { href: "/data", label: "All Data", icon: <GridIcon className="nav-icon" /> },
  ];

  return (
    <aside className="sidebar" ref={sidebarRef}>
      <noscript>
        <span className="mobile-menu-fallback" hidden />
      </noscript>
      <div className="sidebar-header">
        <span className="sidebar-title">Dashboard</span>
      </div>

      <div
        id="sidebar-navigation"
        className={`sidebar-navigation${menuOpen ? " is-open" : ""}`}
        onClick={(event) => {
          // Also close when the current page's link is chosen again.
          if (event.target instanceof Element && event.target.closest("a")) closeMenu();
        }}
      >

        <nav aria-label="Main navigation">
          {primary.map((item) => {
            const active = item.href === "/" ? path === "/" : path.startsWith(item.href);
            return (
              <Link key={item.href} href={item.href} aria-current={active ? "page" : undefined} className={`nav-link${active ? " active" : ""}`}>
                {item.icon}
                <span>{item.label}</span>
              </Link>
            );
          })}
        </nav>

        <div className="nav-section">
          <div className="eyebrow" style={{ padding: "0 2px 8px" }}>
            Categories
          </div>
          {GROUPS.map((g) => {
            const href = g === "workouts" ? "/workouts" : `/category/${g}`;
            const active = path === href;
            return (
              <Link key={g} href={href} aria-current={active ? "page" : undefined} className={`nav-link${active ? " active" : ""}`}>
                <GroupIcon group={g} className="nav-icon" size={17} style={{ color: GROUP_COLOR[g] } as React.CSSProperties} />
                <span>{GROUP_LABELS[g]}</span>
              </Link>
            );
          })}
        </div>

        <div className="sidebar-spacer" />

        <div className="nav-section" style={{ marginTop: 0 }}>
          {account && (
            <Link
              href="/account"
              aria-current={path === "/account" ? "page" : undefined}
              className={`nav-link${path === "/account" ? " active" : ""}`}
              title={`Signed in as ${account.email}`}
            >
              <UserIcon className="nav-icon" />
              <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                {account.email}
              </span>
            </Link>
          )}
          {account?.isAdmin && (
            <Link href="/admin" aria-current={path === "/admin" ? "page" : undefined} className={`nav-link${path === "/admin" ? " active" : ""}`}>
              <GridIcon className="nav-icon" />
              <span>Admin</span>
            </Link>
          )}
          <Link href="/settings" className={`nav-link${path === "/settings" ? " active" : ""}`}>
            <SettingsIcon className="nav-icon" />
            <span>Settings</span>
          </Link>
          {account && (
            // A form post, not a link: signing out changes state, and works without JavaScript.
            <form method="post" action="/api/auth/logout">
              <button type="submit" className="nav-link" style={{ width: "100%", cursor: "pointer", background: "transparent" }}>
                <SignOutIcon className="nav-icon" />
                <span>Sign out</span>
              </button>
            </form>
          )}
          <div className="chip" style={{ marginTop: 10, width: "100%", justifyContent: "flex-start" }} title={source.detail}>
            <span
              className="dot"
              style={{ background: source.source === "live" ? "#30d158" : "#ff453a" }}
            />
            <span style={{ fontSize: 12 }}>
              {source.source === "live" ? "Live data" : "Database unavailable"}
            </span>
          </div>
        </div>
        <div className="mobile-site-resources nav-section">
          <nav className="site-public-menu" aria-label="Website resources">
            <div className="site-menu-heading">PulsHealth</div>
            <Link href="/" aria-current="location">Dashboard</Link>
            <a href="https://pulshealth.com/knowledge-base/">Knowledge Base</a>
            <a href="https://pulshealth.com/docs/">Docs</a>
            <a href="https://pulshealth.com/consulting/">Consulting</a>
            <div className="site-menu-heading">More</div>
            <a href="https://pulshealth.com/server/">Self-hosting</a>
            <a href="https://pulshealth.com/support/">Support &amp; FAQ</a>
            <a href="https://pulshealth.com/about/">About</a>
            <a href="https://pulshealth.com/privacy/">Privacy</a>
            <a href="https://github.com/PulsHealth/pulshealth" target="_blank" rel="noopener noreferrer">GitHub</a>
          </nav>
        </div>
      </div>
    </aside>
  );
}
