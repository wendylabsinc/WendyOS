"use client";
import { useEffect, useImperativeHandle, useRef, useState } from "react";
import {
  ArrowUpRight,
  Loader2,
  ShieldCheck,
  RefreshCw,
  Cpu,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { WendyClient } from "@/lib/client";
import DeviceDashboard from "@/components/device-dashboard";
import { AUTH_CALLBACK_TYPE, type AuthSession } from "@/lib/auth";
type CloudDevice = {
  id: string;
  name: string;
  deviceType?: string;
  osType?: string;
  lastHeartbeat?: string;
};
export default function WendyLogin({
  session,
  onSession,
  onSignOut,
  ref,
  onActive,
  requestedView,
}: {
  onActive: (device: { name: string } | null) => void;
  requestedView: string;
  ref?: React.Ref<{ signOut: () => void }>;
  session: AuthSession | null;
  onSession: (s: AuthSession) => void;
  onSignOut: () => void;
}) {
  const [selected, setSelected] = useState<CloudDevice | null>(null);
  useEffect(() => {
    onActive(selected);
  }, [selected, onActive]);
  useEffect(() => {
    if (!session) setSelected(null);
  }, [session]);
  const client = useRef<WendyClient | null>(null);
  const cancel = useRef<(() => void) | null>(null);
  const [devices, setDevices] = useState<CloudDevice[] | null>(null);
  const [discovering, setDiscovering] = useState(false);
  const [cloudError, setCloudError] = useState("");
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [restoring, setRestoring] = useState(true);
  const [error, setError] = useState("");
  const [local, setLocal] = useState<boolean | null>(null);
  useEffect(() => {
    setLocal(window.location.origin === "http://localhost:5173");
    void restore();
    return () => {
      cancel.current?.();
      client.current?.dispose();
    };
  }, []);
  async function discover(worker = client.current) {
    if (!worker) return;
    setDiscovering(true);
    setCloudError("");
    try {
      const found = await worker.call<CloudDevice[]>("cloud-discover");
      if (client.current === worker) setDevices(found);
    } catch (e) {
      if (client.current === worker)
        setCloudError(e instanceof Error ? e.message : String(e));
    } finally {
      if (client.current === worker) setDiscovering(false);
    }
  }
  function attach(worker: WendyClient) {
    worker.onEvent(({ event, data }) => {
      if (client.current !== worker) return;
      if (event === "auth-profile") onSession(data as AuthSession);
      if (event === "auth-signed-out") {
        cancel.current?.();
        worker.dispose();
        client.current = null;
        setRestoring(false);
        setBusy(false);
        setDiscovering(false);
        setDevices(null);
        setCloudError("");
        onSignOut();
      }
    });
  }
  async function restore() {
    setRestoring(true);
    setError("");
    client.current?.dispose();
    const worker = new WendyClient();
    client.current = worker;
    attach(worker);
    try {
      const profile = await worker.call<AuthSession | null>("auth-restore");
      if (client.current !== worker) return;
      if (profile) {
        onSession(profile);
        void discover(worker);
      }
    } catch (e) {
      if (client.current === worker)
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (client.current === worker) setRestoring(false);
    }
  }
  async function signOut() {
    cancel.current?.();
    setBusy(true);
    const worker = client.current;
    try {
      if (worker) await worker.call("auth-signout");
      worker?.dispose();
      client.current = null;
      setDevices(null);
      setCloudError("");
      setDiscovering(false);
      setError("");
      setRestoring(false);
      onSignOut();
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      setError(message);
      setCloudError(message);
    } finally {
      setBusy(false);
    }
  }
  useImperativeHandle(ref, () => ({ signOut }));
  async function signIn(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    const popup = window.open(
      "about:blank",
      "wendy-sign-in",
      "popup,width=520,height=740",
    );
    if (!popup) {
      setError("Allow pop-ups for this site, then try signing in again.");
      return;
    }
    popup.document.title = "Signing in to Wendy";
    popup.document.body.textContent = "Preparing secure sign-in…";
    setBusy(true);
    client.current?.dispose();
    const worker = new WendyClient();
    client.current = worker;
    attach(worker);
    let completing = false;
    let timer: ReturnType<typeof setInterval> | undefined;
    const cleanup = () => {
      window.removeEventListener("message", message);
      if (timer) clearInterval(timer);
      cancel.current = null;
    };
    const fail = (reason: string) => {
      cleanup();
      popup.close();
      worker.dispose();
      if (client.current === worker) client.current = null;
      setError(reason);
      setBusy(false);
    };
    const message = async (event: MessageEvent) => {
      if (
        event.origin !== window.location.origin ||
        event.source !== popup ||
        event.data?.type !== AUTH_CALLBACK_TYPE ||
        completing
      )
        return;
      completing = true;
      cleanup();
      popup.close();
      if (event.data.error) {
        fail("Sign-in was not completed: " + event.data.error);
        return;
      }
      try {
        const profile = await worker.call<AuthSession>("auth-complete", {
          code: event.data.code || "",
          state: event.data.state || "",
          issuer: event.data.issuer || "",
        });
        onSession(profile);
        setBusy(false);
        void discover(worker);
      } catch (e) {
        fail(e instanceof Error ? e.message : String(e));
      }
    };
    cancel.current = () => {
      cleanup();
      popup.close();
    };
    window.addEventListener("message", message);
    const started = Date.now();
    timer = setInterval(() => {
      if (!completing && (popup.closed || Date.now() - started > 600000))
        fail(
          popup.closed
            ? "Sign-in window closed. Try again when you are ready."
            : "Sign-in expired. Please try again.",
        );
    }, 500);
    try {
      const url = await worker.call<string>("auth-begin", {
        email: email.trim(),
        redirect: window.location.origin + "/auth/callback",
      });
      if (popup.closed) return;
      popup.location.href = url;
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    }
  }
  return (
    <>
      {!session && (
        <section className="login-panel panel" aria-label="Wendy account">
          <div className="login-icon">
            <ShieldCheck size={25} />
          </div>
          <div className="login-copy">
            <h2>Connect your Wendy account</h2>
            <p className="subtle">
              Your sign-in is saved in this browser until you sign out.
            </p>
            {local === false && (
              <p className="field-note">
                Sign-in is currently registered for{" "}
                <a href="http://localhost:5173">localhost:5173</a>. This hosted
                address needs an auth callback registration.
              </p>
            )}
            {error && (
              <div className="inline-error" role="alert">
                <p>{error}</p>
                <Button
                  type="button"
                  variant="ghost"
                  disabled={restoring}
                  onClick={() => void restore()}
                >
                  Retry saved sign-in
                </Button>
              </div>
            )}
          </div>
          <form onSubmit={signIn} className="login-form">
            <Input
              aria-label="Wendy email"
              type="email"
              placeholder="you@company.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              disabled={busy || restoring}
              required
              autoComplete="email"
            />
            <Button
              type="submit"
              disabled={busy || restoring || local !== true}
            >
              {busy || restoring ? (
                <Loader2 className="animate-spin" />
              ) : (
                <ArrowUpRight />
              )}
              {restoring
                ? "Restoring sign-in…"
                : busy
                  ? "Signing in…"
                  : "Sign in with Wendy"}
            </Button>
            {busy && (
              <Button type="button" variant="ghost" onClick={signOut}>
                Cancel
              </Button>
            )}
          </form>
        </section>
      )}
      {session && selected && client.current && (
        <DeviceDashboard
          key={selected.id}
          device={selected}
          client={client.current}
          requestedView={requestedView}
          onBack={() => setSelected(null)}
        />
      )}
      {session && !selected && (
        <section className="panel cloud-inventory">
          <div className="panel-heading">
            <div>
              <h2>Online cloud devices</h2>
              <p className="subtle">
                Devices currently connected to Wendy Cloud
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={discovering}
              onClick={() => void discover()}
            >
              <RefreshCw className={discovering ? "animate-spin" : ""} />
              Refresh
            </Button>
          </div>
          {cloudError && (
            <div className="inline-error" role="alert">
              {cloudError}
            </div>
          )}
          {discovering && devices === null && (
            <p className="subtle p-5">Loading your devices…</p>
          )}
          {devices?.length === 0 && (
            <p className="subtle p-5">No cloud devices are online.</p>
          )}
          {!!devices?.length && (
            <div className="cloud-device-list">
              {devices.map((d) => (
                <button
                  type="button"
                  className="cloud-device-row"
                  key={d.id}
                  onClick={() => setSelected(d)}
                >
                  <Cpu size={20} />
                  <div>
                    <strong>{d.name || "Unnamed device"}</strong>
                    <p className="subtle">
                      {d.deviceType || d.osType || "Wendy device"}
                    </p>
                  </div>
                  <span className="tag live">Online</span>
                  <span className="subtle">
                    {d.lastHeartbeat
                      ? "Last seen " +
                        new Date(d.lastHeartbeat).toLocaleString()
                      : "No heartbeat reported"}
                  </span>
                  <ArrowUpRight size={18} />
                </button>
              ))}
            </div>
          )}
          <p className="field-note p-5">
            Select a device to open its live dashboard and OpenTelemetry data.
          </p>
        </section>
      )}
    </>
  );
}
