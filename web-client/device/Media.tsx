import { useEffect, useRef, useState } from "react";
import type { WendyClient } from "@/lib/client";
import type { StreamEvent } from "./rpc";
export default function Media({ client, stream, audio, onReady }: { client: WendyClient; stream: string; audio: boolean; onReady: (error?: string) => void }) {
  const video = useRef<HTMLVideoElement>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let source: SourceBuffer | undefined, media: MediaSource | undefined, url = "";
    let context: AudioContext | undefined, nextTime = 0, queuedBytes = 0;
    let stopped = false;
    const queue: Uint8Array<ArrayBuffer>[] = [];
    function fail(message: string) {
      if (stopped) return;
      stopped = true; setError(message); onReady(message);
      void client.call("device-stream-stop", { stream }).catch(() => {});
    }
    function pump() {
      if (!source || source.updating || stopped || !queue.length) return;
      try {
        if (video.current && source.buffered.length && video.current.currentTime - source.buffered.start(0) > 20) {
          source.remove(source.buffered.start(0), video.current.currentTime - 10); return;
        }
        const chunk = queue.shift()!; queuedBytes -= chunk.length;
        source.appendBuffer(chunk);
      } catch (e) { fail(String(e)); }
    }
    if (audio) {
      context = new AudioContext();
      void context.resume().catch((e) => fail(String(e)));
    } else if (typeof MediaSource === "undefined" || !MediaSource.isTypeSupported('video/webm; codecs="vp8"')) {
      fail("This browser does not support the device's VP8 preview. Use a browser with WebM playback support.");
    } else {
      media = new MediaSource(); url = URL.createObjectURL(media);
      if (video.current) video.current.src = url;
      media.addEventListener("sourceopen", () => {
        if (stopped) return;
        try {
          source = media!.addSourceBuffer('video/webm; codecs="vp8"');
          source.addEventListener("updateend", pump);
          source.addEventListener("error", () => fail("The browser could not decode this camera stream."));
          pump();
        } catch (e) { fail(String(e)); }
      }, { once: true });
    }
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (event !== "device-stream" || item.stream !== stream || stopped || !item.message) return;
      const message = item.message as { data?: string; pcmData?: string; sampleRate?: number; channels?: number };
      const encoded = audio ? message.pcmData : message.data;
      if (!encoded) return;
      const bytes = Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0));
      if (audio && context) {
        const channels = message.channels || 1, sampleRate = message.sampleRate || 48000;
        const frames = Math.floor(bytes.length / 2 / channels);
        if (!frames) return;
        const buffer = context.createBuffer(channels, frames, sampleRate), view = new DataView(bytes.buffer);
        for (let c = 0; c < channels; c++) {
          const samples = buffer.getChannelData(c);
          for (let i = 0; i < frames; i++) samples[i] = view.getInt16((i * channels + c) * 2, true) / 32768;
        }
        if (nextTime > context.currentTime + 2) return;
        const node = context.createBufferSource(); node.buffer = buffer; node.connect(context.destination);
        nextTime = Math.max(nextTime, context.currentTime + .03); node.start(nextTime); nextTime += buffer.duration;
      } else {
        queuedBytes += bytes.length;
        if (queuedBytes > 8 * 1024 * 1024) { fail("Video playback fell behind. Stop and restart the preview."); return; }
        queue.push(bytes); pump();
      }
    });
    if (!stopped) onReady();
    return () => {
      stopped = true; off(); void context?.close();
      if (url) URL.revokeObjectURL(url);
    };
  }, [client, stream, audio, onReady]);
  return <div className="device-media">{error && <p role="alert" className="inline-error">{error}</p>}{audio ? <p>Listening to the device microphone. Stop the stream to end playback.</p> : <video ref={video} autoPlay muted playsInline controls />}</div>;
}
