export type Priority = "foreground" | "background";

type Job = {
  run: () => Promise<void>;
  signal?: AbortSignal;
  cancel: () => void;
};

// The stdio tunnel serves one request at a time. Keep background discovery here
// so it cannot fill the tunnel ahead of the next user action.
export class RequestQueue {
  private foreground: Job[] = [];
  private background: Job[] = [];
  private active = false;
  private timer?: ReturnType<typeof setTimeout>;

  enqueue<T>(
    run: () => Promise<T>,
    priority: Priority = "foreground",
    signal?: AbortSignal,
  ): Promise<T> {
    return new Promise((resolve, reject) => {
      const abortError = () =>
        signal?.reason ?? new DOMException("Request canceled", "AbortError");
      if (signal?.aborted) {
        reject(abortError());
        return;
      }
      const job: Job = {
        signal,
        cancel: () => {
          this.foreground = this.foreground.filter((item) => item !== job);
          this.background = this.background.filter((item) => item !== job);
          reject(abortError());
        },
        run: async () => {
          signal?.removeEventListener("abort", job.cancel);
          if (signal?.aborted) {
            reject(abortError());
            return;
          }
          try {
            resolve(await run());
          } catch (error) {
            reject(error);
          }
        },
      };
      signal?.addEventListener("abort", job.cancel, { once: true });
      this[priority].push(job);
      this.pump();
    });
  }

  private pump() {
    if (this.active) return;
    if (this.foreground.length) {
      clearTimeout(this.timer);
      this.timer = undefined;
      this.start(this.foreground.shift()!);
    } else if (this.background.length && !this.timer) {
      // Yield once so a user action in this turn can take priority, without
      // adding a fixed delay to every live preview or pose snapshot.
      this.timer = setTimeout(() => {
        this.timer = undefined;
        const job = this.foreground.shift() ?? this.background.shift();
        if (job) this.start(job);
      }, 0);
    }
  }

  private start(job: Job) {
    this.active = true;
    void job.run().finally(() => {
      this.active = false;
      this.pump();
    });
  }
}
