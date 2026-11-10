"use client";

import { useEffect, useRef, useState, type ComponentProps, type ReactNode } from "react";
import { usePathname } from "next/navigation";
import { SiteHeader } from "./SiteHeader";
import { Sidebar } from "./Sidebar";

type Props = Omit<ComponentProps<typeof Sidebar>, "menuOpen" | "sidebarRef" | "closeMenu"> & {
  children: ReactNode;
};

export function AppNavigation({ children, ...sidebar }: Props) {
  const pathname = usePathname();
  const [menu, setMenu] = useState({ pathname, open: false });
  const open = menu.pathname === pathname && menu.open;
  const headerRef = useRef<HTMLElement>(null);
  const sidebarRef = useRef<HTMLElement>(null);
  const toggleRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const close = () => setMenu({ pathname, open: false });
    const dismiss = (event: PointerEvent) => {
      if (event.target instanceof Node && !headerRef.current?.contains(event.target) && !sidebarRef.current?.contains(event.target)) close();
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        close();
        toggleRef.current?.focus();
      }
    };
    // A mobile menu should not reopen after visiting the desktop layout.
    const desktop = window.matchMedia("(min-width: 1024px)");
    const resize = () => { if (desktop.matches) close(); };
    document.addEventListener("pointerdown", dismiss);
    document.addEventListener("keydown", escape);
    desktop.addEventListener("change", resize);
    return () => {
      document.removeEventListener("pointerdown", dismiss);
      document.removeEventListener("keydown", escape);
      desktop.removeEventListener("change", resize);
    };
  }, [open, pathname]);

  return (
    <>
      <SiteHeader open={open} toggleMenu={() => setMenu({ pathname, open: !open })} headerRef={headerRef} toggleRef={toggleRef} />
      <div className="shell">
        <Sidebar {...sidebar} menuOpen={open} sidebarRef={sidebarRef} closeMenu={() => setMenu({ pathname, open: false })} />
        <main className="content">
          {children}
        </main>
      </div>
    </>
  );
}
