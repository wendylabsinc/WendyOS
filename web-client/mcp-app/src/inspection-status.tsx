export type InspectionPhase = "waiting" | "loading" | "complete" | "error";

type InspectionFacts = {
  connected: boolean;
  agent_version?: string;
  observed_at?: string;
  apps?: unknown[];
};

export function inspectionLabel(phase: InspectionPhase, connected?: boolean) {
  if (phase === "loading") return "Checking agent";
  if (phase === "waiting") return "Waiting for host";
  if (phase === "error") return "Inspection failed";
  return connected === true
    ? "Agent responded"
    : "Agent connection unavailable";
}

export function InspectionStatus({
  phase,
  inspection,
  error,
  ready,
  onRetry,
}: {
  phase: InspectionPhase;
  inspection?: InspectionFacts;
  error: string;
  ready: boolean;
  onRetry: () => void;
}) {
  const loading = phase === "loading";
  const waiting = loading || phase === "waiting";
  const confirmed = phase === "complete" && inspection?.connected === true;
  const checkedAt = inspection?.observed_at
    ? new Date(inspection.observed_at)
    : undefined;
  return (
    <div className="device-facts">
      <span className={"badge" + (confirmed ? " online" : "")} role="status">
        {loading && <span className="inspection-spinner" aria-hidden="true" />}
        {inspectionLabel(phase, inspection?.connected)}
      </span>
      <h2>Device status</h2>
      {waiting ? (
        <p className="inspection-hint">
          {loading
            ? "Waiting for the Wendy Agent. You can use other tabs or select another device."
            : "Device inspection starts when ChatGPT connects to Wendy."}
        </p>
      ) : phase === "error" ? (
        <p className="inspection-error" role="alert">
          {error}
        </p>
      ) : !confirmed ? (
        <p className="inspection-hint">
          The agent did not confirm a connection.
        </p>
      ) : null}
      <dl aria-busy={loading}>
        <dt>Agent</dt>
        <dd>
          {waiting ? "Checking…" : inspection?.agent_version || "Unavailable"}
        </dd>
        <dt>Applications</dt>
        <dd>
          {waiting ? "Checking…" : (inspection?.apps?.length ?? "Unavailable")}
        </dd>
        <dt>Last checked</dt>
        <dd>
          {checkedAt && Number.isFinite(checkedAt.getTime())
            ? checkedAt.toLocaleTimeString()
            : "Not checked"}
        </dd>
      </dl>
      <button
        className="inspection-retry"
        disabled={!ready || loading}
        onClick={onRetry}
      >
        {loading
          ? "Checking agent…"
          : phase === "error"
            ? "Retry inspection"
            : "Check agent"}
      </button>
    </div>
  );
}
