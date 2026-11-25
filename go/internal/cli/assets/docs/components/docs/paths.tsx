import { ArrowRight } from 'lucide-react';
import type { ReactNode } from 'react';

// Routes out of a page, drawn as branches off one line so readers pick the one
// that matches their situation.
export function Paths({ children }: { children: ReactNode }) {
  return <div className="not-prose my-6">{children}</div>;
}

type PathProps = {
  when: string;
  title: string;
  href: string;
  src?: string;
  alt?: string;
  children: ReactNode;
};

// The ::before line is the trunk and ::after is this path's branch. The trunk
// stops at the last branch. prepare-content rewrites href and src when they are
// written in MDX, so both are already correct for the deployed base path here.
export function Path({ when, title, href, src, alt = '', children }: PathProps) {
  return (
    <div className="relative pb-4 pl-6 before:absolute before:bottom-0 before:left-0 before:top-0 before:border-l-2 before:border-fd-muted-foreground/40 after:absolute after:left-0 after:top-[calc(50%-0.5rem)] after:w-4 after:border-t-2 after:border-fd-muted-foreground/40 last:pb-0 last:before:bottom-1/2 last:after:top-1/2 sm:pl-8 sm:after:w-6">
      <a
        href={href}
        className="group flex flex-col overflow-hidden border bg-fd-card transition-colors hover:border-fd-primary/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fd-primary focus-visible:ring-offset-2 focus-visible:ring-offset-fd-background sm:flex-row"
      >
        {src ? (
          <img
            src={src}
            alt={alt}
            className="aspect-video w-full bg-fd-muted object-cover object-center sm:w-56 sm:shrink-0"
            loading="lazy"
          />
        ) : null}
        <div className="flex flex-1 flex-col justify-center gap-1 p-4 sm:p-5">
          <p className="text-sm text-fd-muted-foreground">{when}</p>
          <p className="font-semibold text-fd-card-foreground">{title}</p>
          <p className="text-sm text-fd-muted-foreground">{children}</p>
        </div>
        <ArrowRight
          aria-hidden="true"
          className="mr-5 hidden size-5 shrink-0 self-center text-fd-muted-foreground transition-transform group-hover:translate-x-1 group-hover:text-fd-primary sm:block"
        />
      </a>
    </div>
  );
}
