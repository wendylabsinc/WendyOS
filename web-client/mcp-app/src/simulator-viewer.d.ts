declare module "*go2_sim/viewer.js" {
  export class SandboxViewer {
    constructor(
      canvas: HTMLCanvasElement,
      status: HTMLElement,
      options?: {
        request?: (path: string) => Promise<unknown>;
        frameInterval?: number;
        pollAfterResponse?: boolean;
        replayHistory?: boolean;
        viewOffset?: number[];
        lidar?: boolean;
        wheelZoom?: boolean;
      },
    );
    readonly lastSequence: number;
    setActive(active: boolean): void;
    setFollowRobot(enabled: boolean): void;
    resetView(): void;
    zoom(scale: number): void;
    dispose(): void;
  }
}
