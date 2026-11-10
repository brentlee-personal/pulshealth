import type { Metadata, Viewport } from "next";
import { headers } from "next/headers";
import { connection } from "next/server";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";
import "./globals.css";
import "@/components/navigation.css";
import { AppNavigation } from "@/components/AppNavigation";
import { UnitsProvider } from "@/components/UnitsProvider";
import { getDataSource } from "@/lib/queries";
import { TimeZoneProvider } from "@/components/TimeZoneProvider";
import { reportingTimeZone } from "@/lib/reportingTimeZone";
import { configuredTimeZone } from "@/lib/config";
import { currentSession } from "@/lib/viewer";
import { isBarePage, PATH_HEADER } from "@/lib/shell";

export const metadata: Metadata = {
  title: "PulsHealth",
  description: "A sleek window into your self-hosted HealthKit data.",
  // Someone's health records: never for a search index, in any mode.
  robots: { index: false, follow: false },
};

export const viewport: Viewport = {
  themeColor: "#0b1018",
};

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  // The data pages are force-dynamic, but the static /_not-found route still
  // prerendered this layout at build time — with no DATABASE_URL in the image
  // builder, that baked a "Database unavailable" chip (and the build-time zone)
  // into every unmatched URL. Defer to request time so the sidebar status is
  // always live.
  await connection();
  // proxy.ts puts a fresh nonce in the Content-Security-Policy; the one inline
  // script here must carry it or the browser refuses to run it.
  const requestHeaders = await headers();
  const nonce = requestHeaders.get("x-nonce") ?? undefined;
  const session = await currentSession().catch(() => null);
  const timeZone = session ? await reportingTimeZone(session.userId) : configuredTimeZone();
  const runtimeScript = `window.__PULS_TIME_ZONE__=${JSON.stringify(timeZone).replace(/</g, "\\u003c")};`;
  return (
    <html lang="en" data-theme="dark" suppressHydrationWarning className={`${GeistSans.variable} ${GeistMono.variable}`}>
      <head>
        <script nonce={nonce} dangerouslySetInnerHTML={{ __html: runtimeScript }} />
      </head>
      <body>
        <div className="app-bg" />
        <TimeZoneProvider timeZone={timeZone}><UnitsProvider>{await shell(children, requestHeaders.get(PATH_HEADER))}</UnitsProvider></TimeZoneProvider>
      </body>
    </html>
  );
}

async function shell(children: React.ReactNode, pathname: string | null) {
  if (isBarePage(pathname)) return <main className="auth-shell">{children}</main>;
  const session = await currentSession().catch(() => null);
  if (!session) return <main className="auth-shell">{children}</main>;
  const source = await getDataSource();
  return <AppNavigation source={source} account={{ email: session.email, isAdmin: session.isAdmin }}>{children}</AppNavigation>;
}
