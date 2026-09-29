/** Turn transport errors into messages suitable for device monitoring views.
 * @param {unknown} error
 */
export function monitoringError(error) {
  let message = error instanceof Error ? error.message : typeof error === "string" ? error : "";
  message = message.replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "").trim();
  const wrapper = /^(?:Error invoking remote method ['"][^'"]+['"]:\s*|(?:Error|RpcError|ConnectError):\s*|[✗×✖]\s*|(?:simulator|device) unavailable:\s*)/i;
  while (wrapper.test(message)) message = message.replace(wrapper, "").trim();
  const unreachable = /ECONNREFUSED|ENETUNREACH|EHOSTUNREACH|ENOTFOUND|connection refused|connection reset|no route to host|no such host|failed to connect|unable to connect|cannot connect|network.*unreachable/i;

  if (/identity (?:changed|mismatch)|pin mismatch|key (?:has )?changed|different device|does not match (?:the )?(?:saved|expected|pinned)|certificate.*mismatch/i.test(message))
    return "The device identity does not match the saved identity. Verify that you selected the intended device before reconnecting.";
  if (/no authenticated endpoint/i.test(message) && unreachable.test(message))
    return "Cannot reach this device. Check that it is running and connected to the network.";
  if (/pinned.*(?:enrolled|authenticated)|no authenticated endpoint|unauthenticated|not authenticated|authentication (?:failed|required)|permission denied|access denied|not authorized|unauthorized|authorization failed|not logged in|login required|sign in to/i.test(message))
    return "Access to this device could not be authenticated. Sign in and confirm that your account has access to the device.";
  if (/certificate.*(?:expired|not yet valid)/i.test(message))
    return "The secure connection has an invalid certificate date. Check the device's date and time and refresh expired access credentials.";
  if (/TLS|SSL|secure handshake|certificate|secure connection/i.test(message))
    return "A secure connection to this device could not be established. Check its date and time and device access settings.";
  if (/timed?\s*out|timeout|deadline exceeded/i.test(message))
    return "The device did not respond in time. Check that it is running and connected, then try again.";
  if (unreachable.test(message))
    return "Cannot reach this device. Check that it is running and connected to the network.";

  // CLI diagnostics can contain commands, environment variables, and stack
  // traces. Keep a short first-line explanation when it is suitable for a UI.
  const firstLine = message.split(/\r?\n/).find((line) => line.trim())?.trim() || "";
  const concise = firstLine.split(/\s+(?:Run|Try|For details|To debug|Debug with|Check with)\s/i)[0].trim();
  if (!concise || /(?:\b(?:ssh|sudo|wendy|curl)\s|WENDY_[A-Z_]+|--[a-z][a-z-]*|\bat\s+\S+\.[cm]?[jt]s:\d)/i.test(concise))
    return "Monitoring is unavailable for this device. Check the connection and try again.";
  return concise.length > 240 ? `${concise.slice(0, 237).trimEnd()}…` : concise;
}
