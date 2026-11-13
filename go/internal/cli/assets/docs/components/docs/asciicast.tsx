'use client';

import '@xterm/xterm/css/xterm.css';
import type { ITheme, Terminal } from '@xterm/xterm';
import { Pause, Play, RotateCcw } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * Plays an asciinema recording (an asciicast file) in the page.
 *
 * The official asciinema player runs its terminal emulator as WebAssembly,
 * which the docs site's Content-Security-Policy blocks. This plays the same
 * recordings through xterm.js, which is plain JavaScript.
 *
 * Record with `asciinema rec`; see screenshots/README.md.
 */

// The CLI chooses its colours for the terminal it runs in, and recordings are
// made on a dark background, so the player is dark in both site themes. The
// palette matches screenshots/themes/wendy-dark.json.
const theme: ITheme = {
  background: '#121212',
  foreground: '#ebebeb',
  cursor: '#34d399',
  selectionBackground: '#3a3a3a',
  black: '#1a1a1a',
  brightBlack: '#a3a3a3',
  red: '#ef4444',
  brightRed: '#f87171',
  green: '#10b981',
  brightGreen: '#34d399',
  yellow: '#f59e0b',
  brightYellow: '#fbbf24',
  blue: '#0ea5e9',
  brightBlue: '#38bdf8',
  magenta: '#8b5cf6',
  brightMagenta: '#a78bfa',
  cyan: '#06b6d4',
  brightCyan: '#22d3ee',
  white: '#d4d4d4',
  brightWhite: '#fafafa',
};

const FONT_SIZE = 14;
// Space between the window edge and the terminal text, in CSS pixels.
const PADDING = 12;
const FONT_FAMILY = 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace';

type Frame = [time: number, data: string];
type Recording = { cols: number; rows: number; frames: Frame[]; duration: number };
type Status = 'loading' | 'ready' | 'playing' | 'paused' | 'ended' | 'failed';

// Reads asciicast v2 (absolute times) and v3 (intervals, `#` comment lines).
// Pauses are shortened only when the recording asks for it (`asciinema rec -i`).
function parseCast(text: string): Recording {
  const lines = text.split('\n').filter((line) => line.trim() !== '' && !line.startsWith('#'));
  const header = JSON.parse(lines[0]);
  const intervals = header.version === 3;
  const limit: number = header.idle_time_limit ?? Infinity;
  const frames: Frame[] = [];
  let clock = 0; // time in the recording
  let previous = 0;
  let played = 0; // time after long pauses are shortened

  for (const line of lines.slice(1)) {
    const [time, code, data] = JSON.parse(line) as [number, string, string];
    clock = intervals ? clock + time : time;
    if (code !== 'o') continue;
    played += Math.min(clock - previous, limit);
    previous = clock;
    frames.push([played, data]);
  }

  return {
    cols: header.width ?? header.term?.cols ?? 80,
    rows: header.height ?? header.term?.rows ?? 24,
    frames,
    duration: played,
  };
}

function clockText(seconds: number) {
  const whole = Math.max(0, Math.floor(seconds));
  return `${Math.floor(whole / 60)}:${String(whole % 60).padStart(2, '0')}`;
}

type AsciicastProps = {
  /** Path to the .cast file. prepare-content prefixes it with the base path. */
  src: string;
  /** What the recording shows, for the window bar and screen readers. */
  title: string;
  /**
   * Play only part of the recording, in seconds. The terminal still starts in
   * the state the recording had reached at `start`, so one long session can be
   * shown as several steps.
   */
  start?: number;
  end?: number;
  /** Seconds into the recording to show before it is played. Defaults to `end`. */
  poster?: number;
  caption?: string;
  /** Size of the recording, to reserve space before it loads. */
  cols?: number;
  rows?: number;
};

export function Asciicast({ src, title, start, end, poster, caption, cols = 80, rows = 24 }: AsciicastProps) {
  const frameRef = useRef<HTMLDivElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const recordingRef = useRef<Recording | null>(null);
  const cursorRef = useRef(0); // index of the next frame to write
  const positionRef = useRef(0); // seconds into the recording
  const rangeRef = useRef({ from: 0, to: 0 }); // the part of the recording that plays
  const rafRef = useRef(0);
  const posterShownRef = useRef(true);

  const [status, setStatus] = useState<Status>('loading');
  const [position, setPosition] = useState(0); // seconds since `start`
  const [duration, setDuration] = useState(0);
  const [size, setSize] = useState<{ scale: number; height: number } | null>(null);

  // Draw the terminal as it looks at `time`, replaying from the start.
  const seek = useCallback((time: number) => {
    const term = termRef.current;
    const recording = recordingRef.current;
    if (!term || !recording) return;
    let index = 0;
    let output = '';
    while (index < recording.frames.length && recording.frames[index][0] <= time) {
      output += recording.frames[index][1];
      index += 1;
    }
    term.reset();
    term.write(output);
    cursorRef.current = index;
    positionRef.current = time;
    setPosition(Math.max(0, time - rangeRef.current.from));
  }, []);

  const stop = useCallback(() => cancelAnimationFrame(rafRef.current), []);

  const play = useCallback(() => {
    const term = termRef.current;
    const recording = recordingRef.current;
    if (!term || !recording) return;
    const { from, to } = rangeRef.current;
    if (posterShownRef.current || positionRef.current >= to) seek(from);
    posterShownRef.current = false;
    const startedAt = performance.now() - positionRef.current * 1000;
    setStatus('playing');

    const tick = (now: number) => {
      const time = Math.min((now - startedAt) / 1000, to);
      let output = '';
      while (cursorRef.current < recording.frames.length && recording.frames[cursorRef.current][0] <= time) {
        output += recording.frames[cursorRef.current][1];
        cursorRef.current += 1;
      }
      if (output) term.write(output);
      positionRef.current = time;
      setPosition(time - from);
      if (time >= to) {
        setStatus('ended');
        return;
      }
      rafRef.current = requestAnimationFrame(tick);
    };
    rafRef.current = requestAnimationFrame(tick);
  }, [seek]);

  const pause = useCallback(() => {
    stop();
    setStatus('paused');
  }, [stop]);

  // Load xterm.js and the recording once the player is near the viewport.
  useEffect(() => {
    const frame = frameRef.current;
    const stage = stageRef.current;
    if (!frame || !stage) return;
    let disposed = false;
    let resize: ResizeObserver | undefined;

    const load = async () => {
      try {
        const [{ Terminal }, response] = await Promise.all([import('@xterm/xterm'), fetch(src)]);
        if (!response.ok) throw new Error(`${response.status} for ${src}`);
        const recording = parseCast(await response.text());
        if (disposed) return;

        const term = new Terminal({
          cols: recording.cols,
          rows: recording.rows,
          fontSize: FONT_SIZE,
          fontFamily: FONT_FAMILY,
          theme,
          disableStdin: true,
          cursorBlink: false,
          cursorInactiveStyle: 'none',
          scrollback: 0,
        });
        term.open(stage);
        // The recording is read-only: keep phones from opening a keyboard.
        term.textarea?.setAttribute('inputmode', 'none');
        term.textarea?.setAttribute('tabindex', '-1');
        term.textarea?.setAttribute('aria-hidden', 'true');

        termRef.current = term;
        recordingRef.current = recording;
        const to = Math.min(end ?? recording.duration, recording.duration);
        const from = Math.min(start ?? 0, to);
        rangeRef.current = { from, to };
        setDuration(to - from);
        seek(poster ?? to);
        posterShownRef.current = true;

        // xterm lays out at a fixed font size; scale that to the column width.
        const fit = () => {
          const screen = stage.querySelector<HTMLElement>('.xterm-screen');
          if (!screen || screen.offsetWidth === 0) return;
          const scale = (frame.clientWidth - PADDING * 2) / screen.offsetWidth;
          setSize({ scale, height: screen.offsetHeight * scale + PADDING * 2 });
        };
        resize = new ResizeObserver(fit);
        resize.observe(frame);
        requestAnimationFrame(fit);

        setStatus('ready');
      } catch {
        if (!disposed) setStatus('failed');
      }
    };

    const visible = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          visible.disconnect();
          void load();
        }
      },
      { rootMargin: '400px' },
    );
    visible.observe(frame);

    return () => {
      disposed = true;
      visible.disconnect();
      resize?.disconnect();
      cancelAnimationFrame(rafRef.current);
      termRef.current?.dispose();
      termRef.current = null;
    };
  }, [src, start, end, poster, seek]);

  const playing = status === 'playing';
  const loaded = status !== 'loading' && status !== 'failed';
  const toggle = () => (playing ? pause() : play());
  const scrub = (time: number) => {
    stop();
    posterShownRef.current = false;
    seek(rangeRef.current.from + time);
    setStatus(time >= duration ? 'ended' : 'paused');
  };

  return (
    <figure aria-label={`Terminal recording: ${title}`} className="not-prose my-6">
      {/* `dark` scopes the site's colour tokens to their dark values inside the player. */}
      <div className="dark overflow-hidden border border-neutral-800 bg-[#121212] text-neutral-400">
        <div className="flex items-center gap-2 border-b border-neutral-800 px-3 py-2">
          <span aria-hidden="true" className="flex gap-1.5">
            <span className="size-2.5 rounded-full bg-neutral-700" />
            <span className="size-2.5 rounded-full bg-neutral-700" />
            <span className="size-2.5 rounded-full bg-neutral-700" />
          </span>
          <span className="truncate font-mono text-xs">{title}</span>
        </div>

        <div
          ref={frameRef}
          className="relative overflow-hidden"
          style={size ? { height: size.height } : { aspectRatio: `${cols * 0.6} / ${rows * 1.2}` }}
        >
          <div
            ref={stageRef}
            className="absolute origin-top-left"
            style={{
              left: PADDING,
              top: PADDING,
              transform: `scale(${size?.scale ?? 1})`,
              visibility: size ? 'visible' : 'hidden',
            }}
          />
          {status === 'ready' || status === 'ended' ? (
            <button
              type="button"
              onClick={play}
              aria-label={status === 'ended' ? `Replay recording: ${title}` : `Play recording: ${title}`}
              className="absolute inset-0 flex items-center justify-center bg-black/20 transition-colors hover:bg-black/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-fd-primary"
            >
              <span className="flex size-14 items-center justify-center rounded-full bg-fd-primary text-fd-primary-foreground shadow-lg">
                {status === 'ended' ? <RotateCcw className="size-6" /> : <Play className="size-6 translate-x-0.5" />}
              </span>
            </button>
          ) : null}
          {status === 'loading' ? (
            <p className="absolute inset-0 flex items-center justify-center text-sm">
              Loading recording…
            </p>
          ) : null}
          {status === 'failed' ? (
            <p className="absolute inset-0 flex items-center justify-center px-4 text-center text-sm">
              This recording could not be loaded.
            </p>
          ) : null}
        </div>

        <div className="flex items-center gap-3 border-t border-neutral-800 px-3 py-2">
          <button
            type="button"
            onClick={toggle}
            disabled={!loaded}
            aria-label={playing ? 'Pause' : 'Play'}
            className="flex size-7 shrink-0 items-center justify-center transition-colors hover:text-neutral-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fd-primary disabled:opacity-40"
          >
            {playing ? <Pause className="size-4" /> : <Play className="size-4" />}
          </button>
          <input
            type="range"
            min={0}
            max={duration || 1}
            step={0.1}
            value={Math.min(position, duration || 1)}
            disabled={!loaded}
            onChange={(event) => scrub(Number(event.target.value))}
            aria-label="Position in recording"
            aria-valuetext={`${clockText(position)} of ${clockText(duration)}`}
            className="h-1 min-w-0 flex-1 cursor-pointer accent-fd-primary disabled:opacity-40"
          />
          <span className="shrink-0 font-mono text-xs tabular-nums">
            {clockText(position)} / {clockText(duration)}
          </span>
        </div>
      </div>
      {caption ? <figcaption className="mt-2 text-sm text-fd-muted-foreground">{caption}</figcaption> : null}
    </figure>
  );
}
