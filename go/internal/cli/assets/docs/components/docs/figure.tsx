import type { ReactNode } from 'react';

type FigureProps = {
  src: string;
  alt: string;
  caption?: string;
};

// A 16:9 picture with an optional caption. The caption is a prop, not children,
// so `wendy docs` drops the whole tag instead of leaving a stray line of text.
// prepare-content rewrites src when it is written in MDX, so it is already
// correct for the deployed base path here.
export function Figure({ src, alt, caption }: FigureProps) {
  return (
    <figure className="not-prose my-6">
      <img
        src={src}
        alt={alt}
        width={1600}
        height={900}
        className="aspect-video h-auto w-full border bg-fd-muted object-cover object-center"
      />
      {caption ? <figcaption className="mt-2 text-sm text-fd-muted-foreground">{caption}</figcaption> : null}
    </figure>
  );
}

// A row of small pictures that show a sequence, such as a robot along a route.
export function Frames({ columns = 2, children }: { columns?: 2 | 3; children: ReactNode }) {
  return (
    <div className={`not-prose my-6 grid gap-3 ${columns === 3 ? 'grid-cols-1 sm:grid-cols-3' : 'grid-cols-2'}`}>
      {children}
    </div>
  );
}

// One picture in <Frames>. Like Figure, the caption is a prop so `wendy docs`
// drops the whole tag.
export function Frame({ src, alt, caption }: FigureProps) {
  return (
    <figure>
      <img
        src={src}
        alt={alt}
        width={1280}
        height={720}
        loading="lazy"
        className="aspect-video h-auto w-full border bg-fd-muted object-cover object-center"
      />
      {caption ? <figcaption className="mt-1.5 text-xs text-fd-muted-foreground">{caption}</figcaption> : null}
    </figure>
  );
}
