"use client";

import { useEffect, type RefObject } from "react";
import Link from "next/link";
import { BrandMark } from "./BrandMark";
import { GitHubIcon } from "./GitHubIcon";

// Match the public site's primary navigation. The viewer is a separately
// deployed app: only Dashboard and account controls use this origin.
const links = [
  { label: "Dashboard", href: "/" },
  { label: "Knowledge Base", href: "https://pulshealth.com/knowledge-base/" },
  { label: "Docs", href: "https://pulshealth.com/docs/" },
  { label: "Consulting", href: "https://pulshealth.com/consulting/" },
];

export function SiteHeader({ open, toggleMenu, headerRef, toggleRef }: {
  open: boolean;
  toggleMenu: () => void;
  headerRef: RefObject<HTMLElement | null>;
  toggleRef: RefObject<HTMLButtonElement | null>;
}) {
  useEffect(() => {
    const search = (event: KeyboardEvent) => {
      if (event.key === "k" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault();
        window.location.assign("https://pulshealth.com/#search");
      }
    };
    document.addEventListener("keydown", search);
    return () => document.removeEventListener("keydown", search);
  }, []);
  return (
    <header className="site-header" ref={headerRef}>
      <div className="site-header-inner">
        <a href="https://pulshealth.com/" className="site-brand"><BrandMark size={28} /><span>PulsHealth</span></a>
        <nav className="site-primary" aria-label="Primary">
          {links.map(item => item.href === "/" ? (
            <Link key={item.label} href="/" className="site-nav-link" aria-current="location">{item.label}</Link>
          ) : (
            <a key={item.label} href={item.href} className="site-nav-link">{item.label}</a>
          ))}
        </nav>
        <div className="site-header-actions">
          <a href="https://pulshealth.com/#search" className="site-action site-outline site-search" aria-label="Search PulsHealth website">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><circle cx="11" cy="11" r="8" /><path d="m21 21-4.3-4.3" /></svg>
            <span>Search</span><kbd><span>⌘</span>K</kbd>
          </a>
          <a href="https://github.com/PulsHealth/pulshealth" className="site-action site-outline site-github" target="_blank" rel="noopener noreferrer" aria-label="PulsHealth on GitHub"><GitHubIcon /><span>GitHub</span></a>
          <button ref={toggleRef} type="button" className="site-action site-menu-toggle" aria-label={open ? "Close navigation menu" : "Open navigation menu"} aria-expanded={open} aria-controls="sidebar-navigation" onClick={toggleMenu}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16" /></svg></button>
        </div>
      </div>
    </header>
  );
}
