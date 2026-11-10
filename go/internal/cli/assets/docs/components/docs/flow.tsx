import type { ReactNode } from 'react';

// A small box-and-arrow diagram written in MDX, so every label stays in the
// page's published Markdown. It runs left to right, and stacks top to bottom
// when its column is too narrow for that.
export function Flow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <figure aria-label={label} className="not-prose @container my-6">
      <div className="flex flex-col @2xl:flex-row @2xl:items-stretch">{children}</div>
    </figure>
  );
}

type FlowNodeProps = {
  title: string;
  note?: string;
  optional?: boolean;
  wide?: boolean;
  children?: ReactNode;
};

export function FlowNode({ title, note, optional = false, wide = false, children }: FlowNodeProps) {
  return (
    <div
      className={`flex min-w-0 flex-col justify-center gap-2 border bg-fd-card p-4 ${wide ? '@2xl:flex-[1.5]' : '@2xl:flex-1'} ${optional ? 'border-dashed' : ''}`}
    >
      <p className="font-semibold text-fd-card-foreground">{title}</p>
      {note ? <p className="text-sm text-fd-muted-foreground">{note}</p> : null}
      {children}
    </div>
  );
}

export function FlowPart({ children }: { children: ReactNode }) {
  return <p className="border bg-fd-background px-3 py-2 text-sm text-fd-foreground">{children}</p>;
}

export function FlowArrow({ optional = false, children }: { optional?: boolean; children: ReactNode }) {
  const line = optional ? 'border-dashed' : 'border-solid';
  return (
    <div className="flex shrink-0 items-center gap-3 pl-8 text-fd-muted-foreground @2xl:w-28 @2xl:flex-col @2xl:justify-center @2xl:gap-1.5 @2xl:px-1.5">
      <span aria-hidden="true" className="flex flex-col items-center @2xl:hidden">
        <span className={`h-8 border-l-2 border-current ${line}`} />
        <svg viewBox="0 0 10 6" className="h-1.5 w-2.5 fill-current">
          <path d="M0 0h10L5 6z" />
        </svg>
      </span>
      <span className="text-xs @2xl:text-center">{children}</span>
      <span aria-hidden="true" className="hidden w-full items-center @2xl:flex">
        <span className={`flex-1 border-t-2 border-current ${line}`} />
        <svg viewBox="0 0 6 10" className="h-2.5 w-1.5 fill-current">
          <path d="M0 0v10l6-5z" />
        </svg>
      </span>
    </div>
  );
}
