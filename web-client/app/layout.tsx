import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Wendy Client",
  description:
    "Connect to Wendy devices, manage applications, and follow live logs from your browser.",
  icons: {
    icon: "/favicon.svg",
    shortcut: "/favicon.svg",
  },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" className="dark">
      <body className="antialiased">{children}</body>
    </html>
  );
}
