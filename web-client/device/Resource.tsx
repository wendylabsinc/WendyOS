import { useEffect, useState } from "react";
import type { WendyClient } from "@/lib/client";
import { failure } from "./rpc";
import { errorMessage } from "./presentation";
export function useResource<T>(client: WendyClient, service: string, method: string, refresh: number) {
  const [state, setState] = useState<{ data?: T; error?: string; loading: boolean }>({ loading: true });
  useEffect(() => {
    let alive = true;
    client.call<T>("device-rpc", { service, method, message: {} }).then((data) => {
      const error = failure(data);
      if (alive) setState(error ? { error, loading: false } : { data, loading: false });
    }).catch((error) => { if (alive) setState({ error: errorMessage(error), loading: false }); });
    return () => { alive = false; };
  }, [client, service, method, refresh]);
  return state;
}
export function ResourceError({ error }: { error?: string }) { return error ? <p className="inline-error" role="alert">{error}</p> : null; }
