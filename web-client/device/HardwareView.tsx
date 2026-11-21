import { useCallback, useEffect, useState, type ComponentType } from "react";
import { Bluetooth, Camera, Mic, Play, Volume2, type LucideProps } from "lucide-react";
import type { WendyClient } from "@/lib/client";
import { useResource, ResourceError } from "./Resource";
import { failure, streamID, type StreamEvent } from "./rpc";
import { errorMessage } from "./presentation";
import { sections } from "./operations";
import OperationCard from "./OperationCard";
import Sheet from "./Sheet";
import Media from "./Media";
import "./HardwareView.css";
const bluetoothService = "wendy.agent.services.v2.WendyBluetoothService";
const cameraService = "wendy.agent.services.v1.WendyVideoService";
const audioService = "wendy.agent.services.v2.WendyAudioService";
type AudioDevice = { deviceId: number; name: string; description?: string; type: string; isDefault?: boolean; volumePercent?: number };
type CameraDevice = { id: number; name: string; model?: string; stableId?: string; path?: string; address?: string; transport?: string; online?: boolean; topic?: string };
type Peripheral = { name?: string; address: string; connected?: boolean; paired?: boolean; rssi?: number };
type Radio = { adapter?: string; available?: boolean; error?: string; meshMode?: string };
function EmptyHardware({ icon: Icon, title, description }: { icon: ComponentType<LucideProps>; title: string; description: string }) {
  return <section className="panel hardware-empty"><Icon size={30} strokeWidth={1.5} /><h2>{title}</h2><p>{description}</p></section>;
}
async function request(client: WendyClient, service: string, method: string, message: Record<string, unknown>) {
  const result = await client.call("device-rpc", { service, method, message });
  const error = failure(result); if (error) throw new Error(error);
  return result;
}
function HardwareAction({ client, section, method, refresh, initial, title, onApplied }: { client: WendyClient; section: string; method: string; refresh: number; initial?: Record<string, unknown>; title?: string; onApplied?: () => void }) {
  const operation = sections.find((item) => item.name === section)!.operations.find((item) => item.method === method)!;
  return <OperationCard client={client} refresh={refresh} operation={{ ...operation, automatic: false, initial: { ...operation.initial, ...initial } }} presentation="header" triggerLabel={title || operation.title} onApplied={onApplied} />;
}
function PreviewFailure({ error, audio = false }: { error: string; audio?: boolean }) {
  const Icon = audio ? Mic : Camera;
  return <section className="hardware-preview-failure" role="status"><Icon size={25} /><h3>{audio ? "Audio unavailable" : "Preview unavailable"}</h3><p>{/v4l2loopback/i.test(error) ? "This WendyOS image is missing the virtual camera driver. Update the device to an image with v4l2loopback support to preview this camera." : audio ? "The microphone stream could not continue. Try again when the device is ready." : "The camera preview could not continue. Try again when the device is ready."}</p><details><summary>Technical details</summary><pre>{error}</pre></details></section>;
}
function StreamPreview({ client, service, method, message, onError, audio = false }: { client: WendyClient; service: string; method: string; message: Record<string, unknown>; onError: (problem: string) => void; audio?: boolean }) {
  const [stream] = useState(streamID), [ready, setReady] = useState(false);
  const encodedMessage = JSON.stringify(message);
  const onReady = useCallback((problem?: string) => { if (problem) onError(problem); else setReady(true); }, [onError]);
  useEffect(() => {
    if (!ready) return;
    let alive = true, failed = false;
    function fail(problem: unknown) {
      if (!alive || failed) return;
      failed = true;
      onError(String(problem));
      void client.call("device-stream-stop", { stream }).catch(() => {});
    }
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (!alive || event !== "device-stream" || item.stream !== stream) return;
      if (item.error) fail(item.error);
      else if (item.done) fail("The preview stream ended.");
    });
    client.call("device-rpc", { service, method, message: JSON.parse(encodedMessage), stream }).then((value) => {
      const problem = failure(value); if (problem) throw new Error(problem);
      if (!alive) void client.call("device-stream-stop", { stream }).catch(() => {});
    }).catch(fail);
    return () => { alive = false; off(); void client.call("device-stream-stop", { stream }).catch(() => {}); };
  }, [client, service, method, encodedMessage, ready, stream, onError]);
  return <Media client={client} stream={stream} audio={audio} onReady={onReady} />;
}
function BluetoothView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0);
  const radio = useResource<Radio>(client, bluetoothService, "GetBluetoothRadio", refresh + revision);
  const [devices, setDevices] = useState<Peripheral[]>([]), [scanning, setScanning] = useState(false), [error, setError] = useState("");
  const [scan, setScan] = useState(""), [acting, setActing] = useState("");
  useEffect(() => {
    if (!scan || radio.error || radio.data?.available === false) return;
    let alive = true;
    const off = client.onEvent(({ event, data }) => {
      const item = data as StreamEvent;
      if (!alive || event !== "device-stream" || item.stream !== scan) return;
      if (item.error) setError(errorMessage(item.error));
      if (item.message) setDevices((previous) => {
        const found = (item.message as { discoveredDevices?: Peripheral[] }).discoveredDevices ?? [];
        const merged = new Map(previous.map((device) => [device.address, device]));
        found.forEach((device) => merged.set(device.address, device)); return [...merged.values()];
      });
      if (item.done) setScanning(false);
    });
    client.call("device-rpc", { service: bluetoothService, method: "ScanBluetoothPeripherals", message: {}, stream: scan }).catch((problem) => { if (alive) { setError(errorMessage(problem)); setScanning(false); } });
    return () => { alive = false; off(); void client.call("device-stream-stop", { stream: scan }).catch(() => {}); };
  }, [client, scan, radio.error, radio.data?.available]);
  const absent = /no bluetooth adapter found/i.test(radio.error || "") || (radio.data?.available === false && !radio.data.adapter);
  async function connect(device: Peripheral) {
    setError(""); setActing(device.address);
    try { const result = await request(client, bluetoothService, device.connected ? "DisconnectBluetoothPeripheral" : "ConnectBluetoothPeripheral", { address: device.address, ...(!device.connected ? { pair: !device.paired } : {}) }) as { paired?: boolean }; setDevices((previous) => previous.map((item) => item.address === device.address ? { ...item, connected: !device.connected, paired: device.connected ? device.paired : result.paired ?? device.paired } : item)); }
    catch (problem) { setError(errorMessage(problem)); } finally { setActing(""); }
  }
  if (radio.loading) return <EmptyHardware icon={Bluetooth} title="Checking Bluetooth" description="Looking for a Bluetooth adapter." />;
  if (absent) return <EmptyHardware icon={Bluetooth} title="Bluetooth unavailable" description="No Bluetooth adapter detected." />;
  if (radio.error) return <ResourceError error={radio.error} />;
  return <div className="hardware-view"><div className="hardware-toolbar"><div><h2>Bluetooth devices</h2><p>{radio.data?.adapter || "Bluetooth adapter"}</p></div><div className="hardware-actions"><button className="primary" onClick={() => { if (scanning) { setScan(""); setScanning(false); } else { setError(""); setScanning(true); setScan(streamID()); } }}>{scanning ? "Stop searching" : "Find devices"}</button><HardwareAction client={client} refresh={refresh} section="Bluetooth" method="ConfigureBluetoothRadio" title="Adapter settings" onApplied={() => setRevision((n) => n + 1)} /></div></div><ResourceError error={error} />
    {devices.length ? <div className="hardware-grid">{devices.map((device) => <article className="panel hardware-card" key={device.address}><Bluetooth size={26} /><h3>{device.name || "Bluetooth device"}</h3><p>{device.connected ? "Connected" : device.paired ? "Paired" : "Nearby"}</p><small>{device.address}</small><div className="hardware-actions"><button className="quiet" disabled={!!acting} onClick={() => void connect(device)}>{acting === device.address ? "Working…" : device.connected ? "Disconnect" : device.paired ? "Connect" : "Pair"}</button>{device.paired && <HardwareAction client={client} refresh={refresh} section="Bluetooth" method="ForgetBluetoothPeripheral" initial={{ address: device.address }} title="Forget" onApplied={() => setDevices((old) => old.filter((item) => item.address !== device.address))} />}</div></article>)}</div> : <EmptyHardware icon={Bluetooth} title={scanning ? "Searching for devices" : "No devices shown"} description={scanning ? "Keep the other device nearby and in pairing mode." : "Choose Find devices to see nearby and paired devices."} />}
  </div>;
}
function AudioView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0), [selected, setSelected] = useState<AudioDevice>(), [volume, setVolume] = useState(0), [error, setError] = useState(""), [busy, setBusy] = useState(false), [listening, setListening] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const handlePreviewError = useCallback((problem: string) => { setPreviewError(problem); setListening(false); }, []);
  const audio = useResource<{ devices?: AudioDevice[] }>(client, audioService, "ListAudioDevices", refresh + revision);
  const devices = audio.data?.devices ?? [];
  async function change(method: string, message: Record<string, unknown>) {
    setError(""); setBusy(true);
    try { await request(client, audioService, method, message); setRevision((n) => n + 1); if (method === "SetDefaultAudioDevice") setSelected((old) => old ? { ...old, isDefault: true } : old); }
    catch (problem) { setError(errorMessage(problem)); } finally { setBusy(false); }
  }
  if (audio.loading) return <EmptyHardware icon={Volume2} title="Checking audio devices" description="Looking for microphones and speakers." />;
  if (audio.error) return <ResourceError error={audio.error} />;
  if (!devices.length) return <EmptyHardware icon={Volume2} title="No audio devices" description="Connect a microphone or speaker to use audio." />;
  return <><div className="hardware-grid">{devices.map((device) => {
    const input = device.type === "AUDIO_DEVICE_TYPE_INPUT", Icon = input ? Mic : Volume2;
    return <button className="panel hardware-card hardware-device-button" key={device.deviceId} onClick={() => { setSelected(device); setVolume(device.volumePercent ?? 0); setListening(false); setPreviewError(""); setError(""); }}><Icon size={26} /><h3>{device.description || device.name || (input ? "Microphone" : "Speaker")}</h3><p>{input ? "Input" : "Output"}{device.isDefault ? " · Default" : ""}</p>{device.volumePercent !== undefined && <strong>{device.volumePercent}% volume</strong>}<span className="hardware-card-link">Audio settings</span></button>;
  })}</div><Sheet open={!!selected} title={selected?.description || selected?.name || "Audio settings"} onClose={() => { setSelected(undefined); setListening(false); }}><ResourceError error={error} />{selected && <><p className="subtle">{selected.type === "AUDIO_DEVICE_TYPE_INPUT" ? "Microphone" : "Speaker"}{selected.isDefault ? " · Default device" : ""}</p>{selected.volumePercent !== undefined && <form className="hardware-volume" onSubmit={(event) => { event.preventDefault(); void change("SetAudioVolume", { deviceId: selected.deviceId, volumePercent: volume }); }}><label htmlFor="audio-volume">Volume <strong>{volume}%</strong></label><input id="audio-volume" type="range" min="0" max="100" value={volume} onChange={(event) => setVolume(Number(event.target.value))} /><button className="primary" disabled={busy}>Apply volume</button></form>}<div className="hardware-actions">{!selected.isDefault && <button className="quiet" disabled={busy} onClick={() => void change("SetDefaultAudioDevice", { deviceId: selected.deviceId })}>Use as default</button>}{selected.type === "AUDIO_DEVICE_TYPE_INPUT" && <button className="quiet" onClick={() => { setPreviewError(""); setListening(!listening); }}>{listening ? "Stop listening" : previewError ? "Retry listening" : "Listen to microphone"}</button>}<HardwareAction client={client} refresh={refresh} section="Audio" method="StreamAudioLevels" initial={{ deviceId: selected.deviceId }} title="Audio levels" /></div>{listening && <StreamPreview client={client} service={audioService} method="StreamAudio" message={{ deviceId: selected.deviceId, sampleRate: 48000, channels: 1 }} audio onError={handlePreviewError} />}{previewError && <PreviewFailure error={previewError} audio />}</>}</Sheet></>;
}
function useCameraDevices(client: WendyClient, refresh: number) {
  const [state, setState] = useState<{ data?: { devices?: CameraDevice[] }; error?: string; loading: boolean }>({ loading: true });
  useEffect(() => {
    let alive = true;
    client.call<{ devices?: CameraDevice[] }>("device-rpc", { service: cameraService, method: "ListVideoDevices", message: { includeUnpaired: true } }).then((data) => {
      if (alive) setState({ data, loading: false });
    }).catch((problem) => { if (alive) setState({ error: errorMessage(problem), loading: false }); });
    return () => { alive = false; };
  }, [client, refresh]);
  return state;
}
function CameraView({ client, refresh }: { client: WendyClient; refresh: number }) {
  const [revision, setRevision] = useState(0), [selected, setSelected] = useState<CameraDevice>(), [watching, setWatching] = useState(false), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const handlePreviewError = useCallback((problem: string) => { setPreviewError(problem); setWatching(false); }, []);
  const cameras = useCameraDevices(client, refresh + revision);
  async function discover() { setBusy(true); setError(""); try { await request(client, cameraService, "RefreshCameras", {}); setRevision((n) => n + 1); } catch (problem) { setError(errorMessage(problem)); } finally { setBusy(false); } }
  const transport = (device: CameraDevice) => ({ VIDEO_TRANSPORT_USB: "USB camera", VIDEO_TRANSPORT_CSI: "Built-in camera", VIDEO_TRANSPORT_IP: "Network camera", VIDEO_TRANSPORT_ROS2: "ROS camera" }[device.transport || ""] || "Camera");
  const devices = cameras.data?.devices ?? [];
  const initial = selected ? { deviceId: selected.id, ...(selected.stableId ? { stableId: selected.stableId } : {}) } : {};
  return <div className="hardware-view"><div className="hardware-toolbar"><div><h2>Cameras</h2><p>{devices.length} {devices.length === 1 ? "camera" : "cameras"} detected</p></div><div className="hardware-actions"><button className="quiet" disabled={busy} onClick={() => void discover()}>{busy ? "Searching…" : "Find cameras"}</button><HardwareAction client={client} refresh={refresh} section="Cameras" method="GetCameraDHCPStatus" title="Network status" /><HardwareAction client={client} refresh={refresh} section="Cameras" method="SetCameraDHCP" title="Camera network" /></div></div><ResourceError error={cameras.error || error} />{cameras.loading ? <EmptyHardware icon={Camera} title="Checking cameras" description="Looking for connected cameras." /> : devices.length ? <div className="hardware-grid hardware-camera-grid">{devices.map((device) => <button className="panel hardware-card hardware-device-button" key={device.stableId || device.id} onClick={() => { setSelected(device); setWatching(true); setPreviewError(""); }}><div className="camera-card-placeholder"><Play size={34} strokeWidth={1.5} /><span>Watch live</span></div><h3>{device.model || device.name || "Camera"}</h3><p>{transport(device)}</p></button>)}</div> : !cameras.error && <EmptyHardware icon={Camera} title="No cameras detected" description="Connect a camera or search for cameras on your network." />}
  <Sheet open={!!selected} title={selected?.model || selected?.name || "Camera"} onClose={() => { setSelected(undefined); setWatching(false); }}>{selected && <><p className="subtle">{transport(selected)}{selected.address ? ` · ${selected.address}` : ""}</p><button className="primary" onClick={() => { setPreviewError(""); setWatching(!watching); }}>{watching ? "Stop preview" : previewError ? "Retry preview" : "Start preview"}</button>{watching && <StreamPreview client={client} service={cameraService} method="StreamVideo" message={{ ...initial, codec: "VIDEO_CODEC_VP8", framerate: 15 }} onError={handlePreviewError} />}{previewError && <PreviewFailure error={previewError} />}<div className="hardware-actions hardware-camera-settings">{selected.path?.startsWith("/dev/video") && <><HardwareAction client={client} refresh={refresh} section="Cameras" method="GetCameraControls" initial={{ deviceId: selected.id }} title="Capture settings" /><HardwareAction client={client} refresh={refresh} section="Cameras" method="SetCameraControls" initial={{ deviceId: selected.id }} title="Adjust capture" /><HardwareAction client={client} refresh={refresh} section="Cameras" method="ResetCameraControls" initial={{ deviceId: selected.id }} title="Reset capture" /></>}{selected.address && <><HardwareAction client={client} refresh={refresh} section="Cameras" method="SetCameraCredentials" initial={{ deviceId: selected.id }} title="Camera sign-in" /><HardwareAction client={client} refresh={refresh} section="Cameras" method="TestCameraCredentials" initial={{ deviceId: selected.id }} title="Test sign-in" /><HardwareAction client={client} refresh={refresh} section="Cameras" method="ForgetCamera" initial={{ deviceId: selected.id }} title="Forget camera" onApplied={() => { setSelected(undefined); setWatching(false); setRevision((n) => n + 1); }} /></>}</div><details className="hardware-technical"><summary>Device details</summary><dl>{selected.path && <><dt>Device path</dt><dd>{selected.path}</dd></>}{selected.stableId && <><dt>Camera ID</dt><dd>{selected.stableId}</dd></>}{selected.topic && <><dt>ROS topic</dt><dd>{selected.topic}</dd></>}</dl></details></>}</Sheet></div>;
}
export default function HardwareView({ client, refresh, section }: { client: WendyClient; refresh: number; section: "Bluetooth" | "Cameras" | "Audio" }) {
  return section === "Bluetooth" ? <BluetoothView client={client} refresh={refresh} /> : section === "Cameras" ? <CameraView client={client} refresh={refresh} /> : <AudioView client={client} refresh={refresh} />;
}
