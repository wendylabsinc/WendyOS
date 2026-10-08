'use client';

import { Play } from 'lucide-react';
import { useState } from 'react';
import { withBasePath } from '@/lib/shared';

type YouTubeVideoProps = {
  id: string;
  title: string;
};

// Shows a thumbnail and loads the YouTube player only when clicked. Fumadocs
// keeps every tab mounted, so a page with a video per tab would otherwise load
// one player per tab. The docs CSP allows same-origin images only, so each
// video needs a local thumbnail at images/youtube/<id>.webp.
export function YouTubeVideo({ id, title }: YouTubeVideoProps) {
  const [playing, setPlaying] = useState(false);

  return (
    <div className="not-prose relative my-4 aspect-video w-full overflow-hidden rounded-lg border bg-fd-muted">
      {playing ? (
        <iframe
          className="absolute inset-0 size-full border-0"
          src={`https://www.youtube-nocookie.com/embed/${id}?autoplay=1&rel=0`}
          title={title}
          referrerPolicy="strict-origin-when-cross-origin"
          allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture; web-share"
          allowFullScreen
        />
      ) : (
        <button
          type="button"
          aria-label={`Play video: ${title}`}
          onClick={() => setPlaying(true)}
          className="group absolute inset-0 size-full cursor-pointer"
        >
          <img
            src={withBasePath(`/images/youtube/${id}.webp`)}
            alt=""
            loading="lazy"
            className="size-full object-cover"
          />
          <span className="absolute inset-0 flex items-center justify-center bg-black/10 transition-colors group-hover:bg-black/25">
            <span className="flex size-16 items-center justify-center rounded-full bg-black/75 text-white shadow-lg transition-transform group-hover:scale-110">
              <Play className="ms-1 size-7 fill-current" aria-hidden="true" />
            </span>
          </span>
        </button>
      )}
    </div>
  );
}
