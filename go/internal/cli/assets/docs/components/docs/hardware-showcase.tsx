import type { ReactNode } from 'react';

export function HardwareShowcase({ children }: { children: ReactNode }) {
  return <div className="not-prose my-8 grid gap-6 sm:grid-cols-2">{children}</div>;
}

type HardwareCardProps = {
  title: string;
  href: string;
  src: string;
  alt: string;
  children: ReactNode;
};

// prepare-content rewrites href and src when they are written in MDX, so both
// are already correct for the deployed base path here.
export function HardwareCard({ title, href, src, alt, children }: HardwareCardProps) {
  return (
    <a
      href={href}
      className="flex flex-col overflow-hidden border bg-fd-card transition-colors hover:border-fd-primary/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fd-primary focus-visible:ring-offset-2 focus-visible:ring-offset-fd-background"
    >
      <img
        src={src}
        alt={alt}
        className="aspect-video w-full bg-fd-muted object-cover object-center"
        loading="lazy"
      />
      <div className="flex flex-1 flex-col gap-1 p-5">
        <p className="font-semibold text-fd-card-foreground">{title}</p>
        <p className="text-sm text-fd-muted-foreground">{children}</p>
      </div>
    </a>
  );
}
