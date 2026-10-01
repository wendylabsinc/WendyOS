"use client";
import { useEffect, useState } from "react";
import { AUTH_CALLBACK_TYPE } from "@/lib/auth";
export default function AuthCallback() {
  const [message, setMessage] = useState("Completing sign-in…");
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const response = {
      type: AUTH_CALLBACK_TYPE,
      code: params.get("code"),
      state: params.get("state"),
      issuer: params.get("iss"),
      error: params.get("error"),
    };
    // Remove authorization codes from the address bar and browser history.
    window.history.replaceState(null, "", "/auth/callback");
    if (!window.opener) {
      setMessage(
        "Return to Wendy Client and start sign-in again. Keep the sign-in window open until it finishes.",
      );
      return;
    }
    window.opener.postMessage(response, window.location.origin);
    setMessage("You can close this window and return to Wendy Client.");
    window.close();
  }, []);
  return (
    <main className="auth-callback">
      <img src="/favicon.svg" alt="Wendy" width="48" />
      <h1>Wendy sign-in</h1>
      <p>{message}</p>
      <a href="/">Return to your workspace</a>
    </main>
  );
}
