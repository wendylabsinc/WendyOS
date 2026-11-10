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
