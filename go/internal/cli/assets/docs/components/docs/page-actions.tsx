'use client';

import { useState } from 'react';
import { withBasePath } from '@/lib/shared';

export function PageActions({ slug }: { slug: string }) {
  const [status, setStatus] = useState('Copy page as Markdown');
  const url = withBasePath(`/markdown/${slug || 'index'}.md`);

  async function copy() {
    try {
      const response = await fetch(url);
      if (!response.ok) throw new Error('Markdown unavailable');
      await navigator.clipboard.writeText(await response.text());
      setStatus('Copied');
    } catch {
      setStatus('Could not copy. Use raw Markdown.');
    }
  }

  return (
    <div className="mb-6 flex flex-wrap gap-x-4 gap-y-2 text-sm text-fd-muted-foreground">
      <button type="button" onClick={copy} className="hover:text-fd-foreground" aria-live="polite">{status}</button>
      <a href={url} className="hover:text-fd-foreground">View raw Markdown</a>
      <a href={withBasePath('/llms.txt')} className="hover:text-fd-foreground">Agent docs index</a>
    </div>
  );
}
