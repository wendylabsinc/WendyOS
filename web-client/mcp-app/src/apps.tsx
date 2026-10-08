import { useEffect, useId, useRef, useState } from "react";
import { app as host, toolErrorMessage } from "./bridge";
import { ErrorCode } from "@modelcontextprotocol/sdk/types.js";

export type GatewayApp = {
  name: string;
  state: string;
  version?: string;
  failure_count?: number;
  readiness?: string;
  can_control?: boolean;
  http_port?: number;
};

export type AppsPanelProps = {
  apps: GatewayApp[];
  canControl: boolean;
  busy: boolean;
  refreshing?: boolean;
  onToggle: (app: GatewayApp) => void | Promise<void>;
  onOpen?: (app: GatewayApp) => Promise<AppWebView | undefined>;
};

export type AppWebView = { url: string; expiresAt: number };

function normalizeState(state: string) {
  return state
    .trim()
    .toUpperCase()
    .replace(/^APP_RUNNING_STATE_/, "");
}

function stateLabel(state: string) {
  if (!state || state === "UNSPECIFIED" || state === "UNKNOWN")
    return "State unavailable";
  return state
    .toLowerCase()
    .replaceAll("_", " ")
    .replace(/^./, (letter) => letter.toUpperCase());
}

function verifiedReadiness(readiness?: string) {
  const value = readiness?.trim().toLowerCase().replaceAll(" ", "_");
  // Only display an explicit readiness result. Container state alone is not one.
  if (value === "ready" || value === "verified_ready") return "Ready";
  if (value === "not_ready" || value === "verified_not_ready")
    return "Not ready";
  return undefined;
}

function ActionIcon({ stop = false }: { stop?: boolean }) {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      {stop ? (
        <rect x="4" y="4" width="8" height="8" fill="currentColor" />
      ) : (
        <path d="M5 2.75 12.5 8 5 13.25Z" fill="currentColor" />
      )}
    </svg>
  );
}

function AppCard({
  app,
  canControl,
  busy,
  onToggle,
  onOpen,
}: Omit<AppsPanelProps, "apps" | "refreshing"> & { app: GatewayApp }) {
  const id = useId();
  const [opening, setOpening] = useState(false);
  const [openError, setOpenError] = useState("");
  const [view, setView] = useState<AppWebView>();
  const [linkStatus, setLinkStatus] = useState("");
  const address = useRef<HTMLInputElement>(null);
  const openingLock = useRef(false);
  const operation = useRef(0);
  useEffect(() => {
    operation.current++;
    openingLock.current = false;
    setOpening(false);
    setView(undefined);
    setLinkStatus("");
    setOpenError("");
    return () => {
      operation.current++;
    };
  }, [app.state, app.version, app.http_port]);
  const state = normalizeState(app.state);
  const running = state === "RUNNING";
  const stopped = state === "STOPPED";
  const knownControlState = running || stopped;
  const allowed = canControl && app.can_control !== false;
  const unavailable = !allowed
    ? "Control access is not enabled for this app."
    : !knownControlState
      ? "Start and stop are unavailable until the app state is confirmed."
      : undefined;
  const pending = busy ? "An action is in progress." : undefined;
  const startReason =
    unavailable ||
    pending ||
    (running ? "This app is already running." : undefined);
  const stopReason =
    unavailable ||
    pending ||
    (stopped ? "This app is already stopped." : undefined);
  const readiness = verifiedReadiness(app.readiness);
  const tone = running
    ? "running"
    : stopped || state === "CRASH_LOOPING"
      ? "stopped"
      : "unknown";

  async function open() {
    if (!running || busy || !onOpen || openingLock.current) return;
    openingLock.current = true;
    const currentOperation = operation.current;
    setOpening(true);
    setOpenError("");
    let prepared = false;
    if (view && view.expiresAt <= Date.now()) setView(undefined);
    try {
      // Retain a prepared URL so a second click opens it directly from the
      // user's gesture and does not allocate another expiring gateway view.
      const next =
        view && view.expiresAt > Date.now() ? view : await onOpen(app);
      if (!next || currentOperation !== operation.current) return;
      prepared = true;
      setView(next);
      setLinkStatus(
        "Web address ready. If no window opened, copy this address into your browser.",
      );
      if (!host.getHostCapabilities()?.openLinks)
        throw Error(
          "This host cannot open browser windows. Use the web address below.",
        );
      const opened = await host.openLink(
        { url: next.url },
        {
          timeout: 5_000,
          maxTotalTimeout: 5_000,
          resetTimeoutOnProgress: false,
        },
      );
      if (opened.isError)
        throw Error(
          "ChatGPT could not open the browser window. Use the web address below.",
        );
    } catch (error) {
      if (currentOperation !== operation.current) return;
      const timedOut =
        typeof error === "object" &&
        error !== null &&
        "code" in error &&
        error.code === ErrorCode.RequestTimeout;
      setOpenError(
        (timedOut
          ? prepared
            ? "ChatGPT did not confirm opening the browser. Use the web address below."
            : "The app did not return a web address in time. Check its logs or try again."
          : toolErrorMessage(error)) ||
          "ChatGPT has not loaded the app-opening tool. Go to Plugins → Wendy → Manage app → Refresh tools, then reopen Wendy.",
      );
    } finally {
      if (currentOperation === operation.current) {
        openingLock.current = false;
        setOpening(false);
      }
    }
  }

  async function copyAddress() {
    if (!view) return;
    try {
      if (!navigator.clipboard) throw Error("Clipboard unavailable");
      await navigator.clipboard.writeText(view.url);
      setLinkStatus("Web address copied.");
    } catch {
      address.current?.focus();
      address.current?.select();
      setLinkStatus(
        "Address selected. Copy it and paste it into your browser.",
      );
    }
  }

  return (
    <li className="app-card">
      <div className="app-card-heading">
        <span className="app-symbol" aria-hidden="true">
          <svg
            viewBox="0 0 24 24"
            width="20"
            height="20"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.3"
            strokeLinejoin="round"
          >
            <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z M4 7.5l8 4.5 8-4.5 M12 12v9 M8 5.25l8 4.5" />
          </svg>
        </span>
        <div className="app-identity">
          <h3>{app.name}</h3>
          {app.version?.trim() && (
            <span className="app-version">{app.version}</span>
          )}
        </div>
      </div>
      <div className="app-status">
        <svg
          className={"app-status-icon " + tone}
          viewBox="0 0 16 16"
          width="16"
          height="16"
          aria-hidden="true"
        >
          <circle
            cx="8"
            cy="8"
            r="6.2"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          />
          {running ? (
            <path
              d="m5.25 8 1.8 1.8 3.7-3.7"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          ) : stopped ? (
            <rect x="5.5" y="5.5" width="5" height="5" fill="currentColor" />
          ) : (
            <path
              d="M8 4.8v4.1 M8 11.1v.1"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          )}
        </svg>
        <span>{stateLabel(state)}</span>
        {readiness && <span className="app-readiness">{readiness}</span>}
      </div>
      {app.failure_count !== undefined && app.failure_count > 0 && (
        <small className="app-failures">
          {app.failure_count} recorded{" "}
          {app.failure_count === 1 ? "failure" : "failures"}
        </small>
      )}
      <div className="app-card-controls">
        <button
          type="button"
          className="app-control"
          disabled={!!startReason}
          aria-label={"Start " + app.name}
          title={startReason || "Start " + app.name}
          aria-describedby={unavailable ? id : undefined}
          onClick={() => {
            if (!startReason) void onToggle(app);
          }}
        >
          <ActionIcon /> Start
        </button>
        <button
          type="button"
          className="app-control"
          disabled={!!stopReason}
          aria-label={"Stop " + app.name}
          title={stopReason || "Stop " + app.name}
          aria-describedby={unavailable ? id : undefined}
          onClick={() => {
            if (!stopReason) void onToggle(app);
          }}
        >
          <ActionIcon stop /> Stop
        </button>
        {app.http_port !== undefined && app.http_port > 0 && (
          <button
            type="button"
            className="app-control app-open"
            disabled={!running || busy || opening || !onOpen}
            aria-label={"Open " + app.name}
            title={
              !running
                ? "Start this app before opening it."
                : !onOpen
                  ? "Opening apps is unavailable in this host."
                  : "Open " + app.name
            }
            onClick={() => void open()}
          >
            {opening ? "Opening…" : "Open app ↗"}
          </button>
        )}
      </div>
      {opening && !view && (
        <p className="app-control-note" role="status">
          Connecting to this app…
        </p>
      )}
      {openError && (
        <p className="error app-open-error" role="alert">
          {openError}
        </p>
      )}
      {view && running && (
        <div className="app-web-address">
          <p role="status">{linkStatus}</p>
          <label htmlFor={id + "-address"}>App web address</label>
          <input
            id={id + "-address"}
            ref={address}
            value={view.url}
            readOnly
            onFocus={(event) => event.currentTarget.select()}
          />
          <button type="button" onClick={() => void copyAddress()}>
            Copy address
          </button>
        </div>
      )}
      {unavailable && (
        <small className="app-control-note" id={id}>
          {unavailable}
        </small>
      )}
    </li>
  );
}

export function AppsPanel({
  apps,
  canControl,
  busy,
  refreshing = false,
  onToggle,
  onOpen,
}: AppsPanelProps) {
  const titleId = useId();
  return (
    <section
      className="apps-panel"
      aria-labelledby={titleId}
      aria-busy={busy || refreshing}
    >
      <div className="apps-panel-heading">
        <h2 id={titleId}>
          Applications <span className="apps-count">{apps.length}</span>
        </h2>
        {refreshing && (
          <span className="apps-refreshing" role="status">
            Refreshing apps...
          </span>
        )}
      </div>
      {apps.length ? (
        <ul className="apps-grid">
          {apps.map((app) => (
            <AppCard
              key={app.name}
              app={app}
              canControl={canControl}
              busy={busy || refreshing}
              onToggle={onToggle}
              onOpen={onOpen}
            />
          ))}
        </ul>
      ) : (
        <div className="apps-empty">
          {refreshing
            ? "Loading applications..."
            : "No visible applications on this device."}
        </div>
      )}
    </section>
  );
}
