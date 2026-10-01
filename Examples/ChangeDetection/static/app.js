"use strict";
const $ = (id) => document.getElementById(id);
const svgNS = "http://www.w3.org/2000/svg";
let state = null;
let selected = null;
let busy = false;
const title = (value) => value.replaceAll("_", " ");
const stamp = (value) =>
  `${String(Math.floor(value / 60)).padStart(2, "0")}:${String(Math.floor(value % 60)).padStart(2, "0")}`;

function svg(name, attributes) {
  const element = document.createElementNS(svgNS, name);
  Object.entries(attributes).forEach(([key, value]) =>
    element.setAttribute(key, value),
  );
  return element;
}

function showError(message) {
  $("error").hidden = !message;
  $("error").textContent = message || "";
}

async function post(action, body = {}) {
  const response = await fetch(`/api/${action}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error((await response.json()).error);
  return response;
}

function renderRegion() {
  if (!state) return;
  const region = state.regions.find((item) => item.id === selected);
  $("empty").hidden = !!region;
  $("region-detail").hidden = !region;
  $("region-id").textContent = region
    ? `REGION ${String(region.id).padStart(2, "0")}`
    : "No selection";
  $("scores").replaceChildren();
  $("decision").classList.toggle("alert", !!region?.alert);
  $("decision").textContent = region?.alert
    ? "Persistent liquid candidate. Review this region."
    : region
      ? "Observing. Liquid alert conditions are not met."
      : "No liquid candidate";
  if (!region) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "Scores appear when a region is detected.";
    $("scores").append(empty);
    return;
  }
  for (const key of ["before", "after", "mask"])
    $("crop-" + key).src = region.images[key];
  const f = region.features;
  $("age").textContent = `${f.age_seconds.toFixed(1)} s`;
  $("growth").textContent = `${(f.growth_rate * 100).toFixed(1)}% / s`;
  $("speed").textContent = `${(f.movement_speed * 100).toFixed(2)}% diag / s`;
  $("export").disabled = !state.paused || !$("label").value;
  $("export-hint").textContent = state.paused
    ? "Choose a human label to export crops, mask, and temporal features."
    : "Pause to save a labeled example.";
  Object.entries(region.scores)
    .sort((a, b) => b[1] - a[1])
    .forEach(([label, score]) => {
      const row = document.createElement("div");
      row.className = "score-row";
      const name = document.createElement("span");
      name.textContent = title(label);
      const track = document.createElement("div");
      track.className = "score-track";
      const fill = document.createElement("div");
      fill.className = "score-fill";
      fill.style.width = `${Math.max(0, Math.min(1, score)) * 100}%`;
      track.append(fill);
      const value = document.createElement("span");
      value.className = "score-value";
      value.textContent = score.toFixed(2);
      row.append(name, track, value);
      $("scores").append(row);
    });
}

function render(data) {
  state = data;
  $("connection").textContent = data.error
    ? "Input stopped"
    : data.paused
      ? "Paused"
      : "Live";
  $("source").textContent = data.source;
  $("classifier").textContent = data.classifier || "Loading classifier";
  $("scenario-control").hidden = !data.demo;
  if (data.scenario) $("scenario").value = data.scenario;
  $("pause").textContent = data.paused ? "Resume" : "Pause";
  $("status").textContent = data.error ? "Input stopped" : data.status;
  $("count").textContent =
    `${data.regions.length} region${data.regions.length === 1 ? "" : "s"}`;
  showError(data.error);
  if (!data.images) return;
  $("clock").textContent = stamp(data.timestamp);
  for (const key of ["before", "current", "residual"])
    $(key).src = data.images[key];
  for (const [id, value, unit] of [
    ["coverage", data.coverage * 100, "%"],
    ["brightness", data.brightness_change_global * 100, "%"],
    ["shift", data.camera_shift_pixels, "px"],
  ]) {
    const suffix = document.createElement("span");
    suffix.textContent = unit;
    $(id).replaceChildren(document.createTextNode(value.toFixed(1)), suffix);
  }
  $("score-kind").textContent = data.score_kind;
  $("model-note").textContent =
    data.score_kind === "heuristic score"
      ? "The rule baseline is untrained. Its scores are not probabilities. Static shadows and stains can resemble liquid."
      : "Model probabilities need calibration on held-out cameras. Ambiguous predictions are labeled unknown.";
  if (!data.regions.some((item) => item.id === selected)) {
    selected = data.regions[0]?.id ?? null;
    $("label").value = "";
  }
  $("region-tabs").replaceChildren();
  $("boxes").replaceChildren();
  $("boxes").setAttribute(
    "viewBox",
    `0 0 ${data.frame_size[0]} ${data.frame_size[1]}`,
  );
  data.regions.forEach((region) => {
    const button = document.createElement("button");
    button.textContent = `#${region.id} ${title(region.label)}`;
    button.className = region.id === selected ? "selected" : "";
    button.setAttribute("aria-pressed", String(region.id === selected));
    button.onclick = () => {
      if (selected !== region.id) $("label").value = "";
      selected = region.id;
      render(state);
    };
    $("region-tabs").append(button);
    const [x, y, width, height] = region.box;
    $("boxes").append(
      svg("rect", {
        x,
        y,
        width,
        height,
        fill: "none",
        stroke: region.alert ? "#f7c56f" : "#e4ffb0",
        "stroke-width": 2,
      }),
    );
    const text = svg("text", {
      x,
      y: Math.max(12, y - 6),
      fill: "#fff",
      "font-size": 12,
      "font-family": "monospace",
      "paint-order": "stroke",
      stroke: "#23372b",
      "stroke-width": 2,
    });
    text.textContent = `#${region.id} ${title(region.label)}`;
    $("boxes").append(text);
  });
  if (!$("label").options.length) {
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = "Choose a label";
    $("label").append(placeholder);
    data.labels.forEach((label) => {
      const option = document.createElement("option");
      option.value = label;
      option.textContent = title(label);
      $("label").append(option);
    });
  }
  $("history").replaceChildren(
    svg("line", { x1: 0, y1: 43, x2: 280, y2: 43, stroke: "#ccd6c7" }),
  );
  const points = data.history
    .map(
      (point, i) =>
        `${(i / Math.max(1, data.history.length - 1)) * 280},${43 - point.liquid * 40}`,
    )
    .join(" ");
  $("history").append(
    svg("polyline", {
      points,
      fill: "none",
      stroke: "#327657",
      "stroke-width": 2,
    }),
  );
  $("events").replaceChildren();
  if (!data.events.length) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "No persistent liquid candidates in this session.";
    $("events").append(empty);
  }
  data.events.forEach((event) => {
    const row = document.createElement("div");
    row.className = "event";
    [
      stamp(event.time),
      event.message,
      `Region ${event.track_id} · score ${event.score.toFixed(2)}`,
    ].forEach((value) => {
      const cell = document.createElement("span");
      cell.textContent = value;
      row.append(cell);
    });
    $("events").append(row);
  });
  renderRegion();
}

async function refresh() {
  const response = await fetch("/api/state", { cache: "no-store" });
  if (!response.ok) throw new Error("Could not read app state");
  render(await response.json());
}

async function control(action, body) {
  if (busy) return;
  busy = true;
  try {
    await post(action, body);
    if (action !== "pause") $("label").value = "";
    await refresh();
  } catch (error) {
    showError(error.message);
  } finally {
    busy = false;
  }
}
$("pause").onclick = () => control("pause", { paused: !state?.paused });
$("reset").onclick = () => control("reset", {});
$("scenario").onchange = () =>
  control("demo", { scenario: $("scenario").value });
$("label").onchange = renderRegion;
$("export").onclick = async () => {
  try {
    const response = await post("export", {
      id: selected,
      label: $("label").value,
    });
    const url = URL.createObjectURL(await response.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = `change-${Date.now()}.zip`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (error) {
    showError(error.message);
  }
};
async function poll() {
  if (!busy) {
    try {
      await refresh();
    } catch (error) {
      $("connection").textContent = "Disconnected";
      showError(error.message);
    }
  }
  setTimeout(poll, 500);
}
poll();
