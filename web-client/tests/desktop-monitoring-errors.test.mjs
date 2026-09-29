import test from "node:test";
import assert from "node:assert/strict";
import { monitoringError } from "../desktop/monitoring-errors.mjs";

test("monitoring errors remove nested Electron and CLI wrappers", () => {
  assert.equal(monitoringError(new Error("Error invoking remote method 'wendy:device:snapshot': Error: ✗ simulator unavailable: This virtual machine is not running.")), "This virtual machine is not running.");
  assert.equal(monitoringError('Error invoking remote method "wendy:device:logs:start": Error: No log entries are available.'), "No log entries are available.");
});

test("TLS diagnostics become concise guidance without commands or debug variables", () => {
  const message = monitoringError("Error invoking remote method 'wendy:device:snapshot': Error: ✗ simulator unavailable: TLS handshake rejected by device (possible clock skew or cert mismatch).\n  Check the device clock: ssh wendy@<host> 'timedatectl status'\n  For full TLS details rerun with WENDY_TLS_DEBUG=1");
  assert.equal(message, "A secure connection to this device could not be established. Check its date and time and device access settings.");
  assert.doesNotMatch(message, /ssh|WENDY_|remote method|wendy:|--device/);
});

test("connection, timeout, authentication, and identity failures remain distinct", () => {
  assert.match(monitoringError("rpc error: deadline exceeded"), /did not respond in time/);
  assert.match(monitoringError("connect ECONNREFUSED 127.0.0.1:50051"), /Cannot reach/);
  assert.match(monitoringError("Pinned to enrolled identity; no authenticated endpoint could be reached"), /Sign in.*access/);
  assert.match(monitoringError('device "127.0.0.1" is pinned to an enrolled identity and no authenticated endpoint answered at 127.0.0.1:50051: connection refused'), /Cannot reach/);
  assert.match(monitoringError("Connection blocked: device identity changed."), /does not match the saved identity/);
  assert.match(monitoringError("x509: certificate has expired"), /invalid certificate date/);
});

test("unknown errors remain readable without command instructions or stack traces", () => {
  assert.equal(monitoringError("Device is busy. Run wendy device top --device vm:lab"), "Device is busy.");
  assert.equal(monitoringError("Log stream ended.\n    at start (service.mjs:42)"), "Log stream ended.");
  assert.doesNotMatch(monitoringError("WENDY_TLS_DEBUG=1 wendy device top"), /WENDY_|wendy/);
  assert.ok(monitoringError("x".repeat(1000)).length <= 240);
  assert.ok(monitoringError(undefined));
});
