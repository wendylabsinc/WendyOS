import { useEffect, useId, useRef, useState } from "react";
import { app, call, share, toolErrorMessage } from "./bridge";

export type Simulator = {
  name: string;
  state: string;
  profile?: string;
  device: string;
};

type Viewer = {
  name: string;
  profile?: string;
  mode?: string;
  url?: string;
  ready: boolean;
  healthy: boolean;
  checkedAt: string;
};

export type SimulatorsPanelProps = {
  enabled: boolean;
  onChanged?: () => void;
};

const namePattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;
const longOperation = { timeout: 1_800_000 };

function viewerURL(value: unknown) {
  if (typeof value !== "string" || !value) return undefined;
  const url = new URL(value);
  if (
    !["http:", "https:"].includes(url.protocol) ||
    url.username ||
    url.password
  ) {
    throw Error("The simulator returned an unsupported viewer URL.");
  }
  return url.href;
}

function profileLabel(profile?: string) {
  return profile === "go2"
    ? "Unitree Go2"
    : profile === "g1"
      ? "Unitree G1"
      : "WendyOS";
}

function stateLabel(state: string) {
  return state
    ? state.replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase())
    : "State unavailable";
}

export function SimulatorsPanel({ enabled, onChanged }: SimulatorsPanelProps) {
  const titleId = useId();
  const nameId = useId();
  const profileId = useId();
  const [simulators, setSimulators] = useState<Simulator[]>([]);
  const [name, setName] = useState("go2-sim");
  const [profile, setProfile] = useState<"go2" | "g1" | "generic">("go2");
  const [busy, setBusy] = useState("");
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [mustRefresh, setMustRefresh] = useState(false);
  const [viewer, setViewer] = useState<Viewer>();
  const [embedded, setEmbedded] = useState(false);
  const [embeddingError, setEmbeddingError] = useState(false);
  const epoch = useRef(0);
  const locked = useRef(false);
  const changed = useRef(onChanged);
  changed.current = onChanged;

  async function readList(current: number) {
    const result = await call("simulator_list");
    const rows = result.structuredContent?.simulators;
    if (!Array.isArray(rows))
      throw Error(
        "Wendy did not return a simulator list. Refresh to try again.",
      );
    const valid = rows.every(
      (row) =>
        row &&
        typeof row === "object" &&
        typeof row.name === "string" &&
        typeof row.state === "string" &&
        typeof row.device === "string",
    );
    if (!valid) throw Error("Wendy returned an incomplete simulator list.");
    if (current !== epoch.current) return;
    const list = rows as Simulator[];
    setSimulators(list);
    setLoaded(true);
    setMustRefresh(false);
    setViewer((previous) => {
      if (
        previous &&
        !list.some(
          (row) =>
            row.name === previous.name && row.state.toLowerCase() === "running",
        )
      ) {
        return undefined;
      }
      return previous;
    });
  }

  async function perform(
    label: string,
    operation: (current: number) => Promise<void>,
    mutation = false,
  ) {
    if (!enabled || locked.current || (mutation && mustRefresh)) return;
    const current = epoch.current;
    locked.current = true;
    setBusy(label);
    setStatus(label);
    setError("");
    try {
      await operation(current);
    } catch (e) {
      if (current !== epoch.current) return;
      const message = toolErrorMessage(e);
      setError(
        mutation && message
          ? `${message} Refresh simulator status before retrying. The operation may have continued on your laptop.`
          : message,
      );
      if (mutation && message) setMustRefresh(true);
      setStatus("");
    } finally {
      if (current === epoch.current) {
        locked.current = false;
        setBusy("");
      }
    }
  }

  function refresh() {
    return perform("Refreshing simulators...", async (current) => {
      await readList(current);
      if (current === epoch.current) setStatus("Simulator status refreshed.");
    });
  }

  async function inspectViewer(simulatorName: string, current: number) {
    const result = await call("simulator_viewer", { name: simulatorName });
    const data = result.structuredContent;
    if (!data) throw Error("Wendy did not return a simulation viewer.");
    const next: Viewer = {
      name: simulatorName,
      profile: typeof data.profile === "string" ? data.profile : undefined,
      mode: typeof data.mode === "string" ? data.mode : undefined,
      url: viewerURL(data.url),
      ready: data.ready === true,
      healthy: data.healthy === true,
      checkedAt: new Date().toLocaleTimeString(),
    };
    if (current === epoch.current) {
      setViewer(next);
      setEmbedded(false);
      setEmbeddingError(false);
    }
    return next;
  }

  async function openBrowser(next: Viewer) {
    if (!next.url)
      throw Error(
        "The simulator has not returned a viewer URL yet. Refresh its status first.",
      );
    const result = await app.openLink({ url: next.url });
    if (result.isError)
      throw Error(
        "ChatGPT could not open the viewer. Use the viewer address below in your browser.",
      );
  }

  async function startSimulator(
    simulatorName: string,
    simulatorProfile: string | undefined,
    current: number,
  ) {
    try {
      await call("simulator_start", { name: simulatorName }, longOperation);
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      if (message.includes("lacks") && message.includes("virtual-robot")) {
        throw Error(
          `${simulatorName} is running, but its agent needs an update for this robot. Refresh status, then choose Finish setup to install the official agent and start the simulation.`,
        );
      }
      throw e;
    }
    if (current !== epoch.current) return;
    changed.current?.();
    await readList(current);
    if (current !== epoch.current) return;
    setStatus(
      `${simulatorName} start finished. Check the reported state below.`,
    );
    if (simulatorProfile === "go2" || simulatorProfile === "g1") {
      // Failure to open a viewer must not make an already completed start look failed.
      try {
        const next = await inspectViewer(simulatorName, current);
        if (current !== epoch.current) return;
        if (next.ready && next.healthy && next.url) await openBrowser(next);
        else
          setStatus(
            `${simulatorName} start finished. Its simulation viewer is not verified ready yet.`,
          );
      } catch (e) {
        if (current === epoch.current)
          setError(
            `The start completed, but the viewer could not open: ${e instanceof Error ? e.message : String(e)}`,
          );
      }
    }
  }

  function create() {
    const simulatorName = name.trim();
    if (
      !namePattern.test(simulatorName) ||
      simulators.some((row) => row.name === simulatorName)
    )
      return;
    void perform(
      `Creating ${simulatorName}. The first image download can take several minutes...`,
      async (current) => {
        await call(
          "simulator_create",
          { name: simulatorName, profile },
          longOperation,
        );
        if (current !== epoch.current) return;
        changed.current?.();
        await readList(current);
        if (current !== epoch.current) return;
        setBusy(`Starting ${simulatorName}...`);
        setStatus(
          `Starting ${simulatorName}. The first launch may download and build the robot profile.`,
        );
        await startSimulator(simulatorName, profile, current);
      },
      true,
    );
  }

  function stop(simulator: Simulator) {
    void perform(
      `Stopping ${simulator.name}...`,
      async (current) => {
        await call("simulator_stop", { name: simulator.name });
        if (current !== epoch.current) return;
        changed.current?.();
        setViewer((previous) =>
          previous?.name === simulator.name ? undefined : previous,
        );
        setEmbedded(false);
        await readList(current);
        if (current === epoch.current)
          setStatus(
            `${simulator.name} stop finished. Check the reported state below.`,
          );
      },
      true,
    );
  }

  useEffect(() => {
    epoch.current++;
    locked.current = false;
    setBusy("");
    setViewer(undefined);
    setEmbedded(false);
    setLoaded(false);
    setError("");
    if (enabled) void refresh();
    return () => {
      epoch.current++;
      locked.current = false;
    };
  }, [enabled]);

  if (!enabled)
    return (
      <section className="simulators-panel" aria-labelledby={titleId}>
        <h1 id={titleId}>Simulators</h1>
        <div className="simulator-setup">
          <h2>Connect Wendy on your laptop</h2>
          <p>
            Local simulators need a Wendy gateway running on your laptop with
            local host access enabled.
          </p>
          <ol>
            <li>Use a local stdio gateway connection.</li>
            <li>
              Enable <code>allow_simulators</code> in the gateway configuration
              and grant <code>simulators:manage</code>.
            </li>
            <li>
              Refresh the Wendy connection in ChatGPT, then reopen Simulators.
            </li>
          </ol>
          <p>
            Once connected, you can create a WendyOS VM and launch a Unitree Go2
            or G1 simulation on your laptop.
          </p>
        </div>
      </section>
    );

  const invalidName = !namePattern.test(name.trim());
  const nameExists = simulators.some((row) => row.name === name.trim());
  const verified = viewer?.ready === true && viewer.healthy === true;

  return (
    <section
      className="simulators-panel"
      aria-labelledby={titleId}
      aria-busy={!!busy}
    >
      <div className="page-title">
        <div>
          <p className="eyebrow">ON YOUR LAPTOP</p>
          <h1 id={titleId}>Simulators</h1>
          <p>Run WendyOS locally and open a real MuJoCo robot simulation.</p>
        </div>
        <button type="button" disabled={!!busy} onClick={() => void refresh()}>
          Refresh status
        </button>
      </div>
      <div className="simulator-progress" role="status">
        {status}
      </div>
      {error && (
        <div className="error" role="alert">
          {error}
        </div>
      )}
      {mustRefresh && (
        <div className="notice">
          Refresh status before starting another operation. No create or start
          request will be retried automatically.
        </div>
      )}
      <div className="simulator-create">
        <div>
          <h2>Create a simulator</h2>
          <p>
            The first launch can take several minutes to download the image and
            build the robot profile.
          </p>
        </div>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            create();
          }}
        >
          <label htmlFor={nameId}>
            Name
            <input
              id={nameId}
              value={name}
              maxLength={32}
              disabled={!!busy}
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              onChange={(event) => setName(event.target.value)}
              aria-describedby={`${nameId}-help`}
            />
          </label>
          <label htmlFor={profileId}>
            Profile
            <select
              id={profileId}
              value={profile}
              disabled={!!busy}
              onChange={(event) =>
                setProfile(event.target.value as typeof profile)
              }
            >
              <option value="go2">Unitree Go2 · MuJoCo</option>
              <option value="g1">Unitree G1 · MuJoCo</option>
              <option value="generic">WendyOS VM</option>
            </select>
          </label>
          <button
            type="submit"
            className="primary"
            disabled={
              !loaded || !!busy || mustRefresh || invalidName || nameExists
            }
          >
            Create and start
          </button>
        </form>
        <small id={`${nameId}-help`}>
          {nameExists
            ? "A simulator with this name already exists. Use its Start button below."
            : "Use 1–32 lowercase letters, numbers, or dashes. Start and end with a letter or number."}
        </small>
      </div>
      <div className="apps-panel-heading">
        <h2>
          Your simulators{" "}
          <span className="apps-count">{simulators.length}</span>
        </h2>
      </div>
      {loaded && !simulators.length && (
        <div className="apps-empty">
          No local simulators yet. Create one above to get started.
        </div>
      )}
      <ul className="simulator-grid">
        {simulators.map((simulator) => {
          const state = simulator.state.toLowerCase();
          const running = state === "running";
          const stopped = state === "stopped";
          const robot =
            simulator.profile === "go2" || simulator.profile === "g1";
          return (
            <li className="simulator-card" key={simulator.name}>
              <div className="simulator-card-heading">
                <span className="simulator-laptop" aria-hidden="true">
                  <svg
                    width="24"
                    height="24"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.3"
                  >
                    <rect x="5" y="4" width="14" height="11" />
                    <path d="M5 15 2 19h20l-3-4 M9 19h6" />
                  </svg>
                </span>
                <div>
                  <h3>{simulator.name}</h3>
                  <p>{profileLabel(simulator.profile)}</p>
                </div>
              </div>
              <div className="simulator-state">
                <span className={"dot " + (running ? "online" : "")} />
                {stateLabel(state)}
              </div>
              <code className="simulator-selector">{simulator.device}</code>
              <div className="simulator-card-controls">
                <button
                  type="button"
                  disabled={
                    !!busy || mustRefresh || (!stopped && !(running && robot))
                  }
                  title={
                    running && robot
                      ? "Connect to the robot runtime and finish any incomplete setup. An already healthy world is preserved."
                      : !stopped
                        ? "Start is available when the simulator is stopped."
                        : undefined
                  }
                  onClick={() =>
                    void perform(
                      `Starting ${simulator.name}. The first launch can take several minutes...`,
                      (current) =>
                        startSimulator(
                          simulator.name,
                          simulator.profile,
                          current,
                        ),
                      true,
                    )
                  }
                >
                  {running && robot ? "▷ Start simulation" : "▷ Start"}
                </button>
                <button
                  type="button"
                  disabled={!!busy || mustRefresh || !running}
                  title={
                    !running
                      ? "Stop is available when the simulator is running."
                      : undefined
                  }
                  onClick={() => stop(simulator)}
                >
                  □ Stop
                </button>
                {robot && (
                  <button
                    type="button"
                    disabled={!!busy || mustRefresh || !running}
                    title="Install the official stable Wendy agent if robot support is missing, restart that simulator's agent, then start the simulation."
                    onClick={() =>
                      void perform(
                        `Finishing ${simulator.name} setup. Checking its agent and installing an official update if needed...`,
                        async (current) => {
                          await call(
                            "simulator_update_agent",
                            { name: simulator.name },
                            longOperation,
                          );
                          if (current !== epoch.current) return;
                          setStatus(`Starting ${simulator.name} simulation...`);
                          await startSimulator(
                            simulator.name,
                            simulator.profile,
                            current,
                          );
                        },
                        true,
                      )
                    }
                  >
                    Finish setup
                  </button>
                )}
                {robot && (
                  <button
                    type="button"
                    disabled={!!busy || !running}
                    onClick={() =>
                      void perform(
                        "Checking simulation viewer...",
                        async (current) => {
                          const next = await inspectViewer(
                            simulator.name,
                            current,
                          );
                          if (current !== epoch.current) return;
                          if (next.ready && next.healthy && next.url) {
                            await openBrowser(next);
                            setStatus("Opened the verified simulation viewer.");
                          } else
                            setStatus(
                              "The simulation viewer is not verified ready yet. Refresh status before trying again.",
                            );
                        },
                      )
                    }
                  >
                    View simulation ↗
                  </button>
                )}
                <button
                  type="button"
                  disabled={!!busy}
                  onClick={() =>
                    void perform(
                      "Sending simulator context to ChatGPT...",
                      async () => {
                        await share(
                          `Help me develop an application for local Wendy simulator ${simulator.name}, device selector ${simulator.device}, profile ${simulator.profile || "generic"}. Inspect its current state first and use this explicit simulator target for any deployment.`,
                        );
                        setStatus("Simulator context sent to ChatGPT.");
                      },
                    )
                  }
                >
                  Develop in ChatGPT
                </button>
              </div>
            </li>
          );
        })}
      </ul>
      {viewer && (
        <section
          className="simulator-viewer"
          aria-label={`Simulation viewer for ${viewer.name}`}
        >
          <div className="simulator-viewer-heading">
            <div>
              <h2>{viewer.name}</h2>
              <p>
                {verified ? "Live simulation" : "Viewer readiness not verified"}
                {viewer.mode ? ` · ${viewer.mode}` : ""}
              </p>
            </div>
            <div className="actions">
              <button
                type="button"
                disabled={!!busy || !viewer.url}
                onClick={() =>
                  void perform("Opening simulation viewer...", async () => {
                    await openBrowser(viewer);
                    setStatus("Viewer opened in the browser.");
                  })
                }
              >
                Open in browser ↗
              </button>
              {verified && viewer.url && (
                <button
                  type="button"
                  disabled={!!busy}
                  onClick={() => {
                    setEmbedded((value) => !value);
                    setEmbeddingError(false);
                  }}
                >
                  {embedded ? "Hide viewer" : "Show here"}
                </button>
              )}
            </div>
          </div>
          <small>
            Viewer checked at {viewer.checkedAt}. ChatGPT may block a localhost
            viewer here; Open in browser remains available.
          </small>
          {viewer.url && (
            <code className="simulator-viewer-address">{viewer.url}</code>
          )}
          {embedded && verified && viewer.url && (
            <iframe
              className="simulator-viewer-frame"
              title={`${viewer.name} live simulation`}
              src={viewer.url}
              sandbox="allow-scripts allow-same-origin allow-forms allow-pointer-lock"
              referrerPolicy="no-referrer"
              onError={() => setEmbeddingError(true)}
            />
          )}
          {embeddingError && (
            <p className="simulator-embed-note">
              The viewer could not load here. Open it in your browser.
            </p>
          )}
        </section>
      )}
    </section>
  );
}
