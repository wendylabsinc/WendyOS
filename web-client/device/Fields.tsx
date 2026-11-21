import { useId } from "react";
import type { Field } from "./rpc";
import { label } from "./rpc";
type Values = Record<string, unknown>;
export default function Fields({ fields, value, onChange }: { fields: Field[]; value: Values; onChange: (next: Values) => void }) {
  const prefix = useId();
  function set(field: Field, next: unknown) {
    const copy = { ...value };
    if (field.oneof) for (const peer of fields) if (peer.oneof === field.oneof) delete copy[peer.name];
    if (next === undefined) delete copy[field.name]; else copy[field.name] = next;
    onChange(copy);
  }
  return <div className="device-fields">{fields.map((field) => {
    const current = value[field.name], id = prefix + field.name;
    if (field.list) {
      const items = (current ?? []) as unknown[];
      return <fieldset key={id}><legend>{label(field.name)}</legend>{items.map((item, index) => <div className="device-list-field" key={index}>
        {field.kind === "message" ? <Fields fields={field.fields ?? []} value={item as Values} onChange={(next) => set(field, items.map((v, i) => i === index ? next : v))} /> :
          <input aria-label={`${label(field.name)} ${index + 1}`} value={String(item)} onChange={(e) => set(field, items.map((v, i) => i === index ? e.target.value : v))} />}
        <button type="button" className="quiet" onClick={() => set(field, items.filter((_, i) => i !== index))}>Remove</button>
      </div>)}<button type="button" className="quiet" onClick={() => set(field, [...items, field.kind === "message" ? {} : ""])}>Add {label(field.name).toLowerCase()}</button></fieldset>;
    }
    if (field.kind === "message" && !field.map) return <details key={id} className="device-nested"><summary>{label(field.name)}{current ? " · changed" : ""}</summary><Fields fields={field.fields ?? []} value={(current ?? {}) as Values} onChange={(next) => set(field, Object.keys(next).length ? next : undefined)} /></details>;
    if (field.map) return <label key={id} htmlFor={id}>{label(field.name)}<textarea id={id} placeholder="KEY=value, one per line" onChange={(e) => {
      const entries = e.target.value.split("\n").filter(Boolean).map((line) => { const split = line.indexOf("="); return [line.slice(0, split), line.slice(split + 1)]; });
      set(field, Object.fromEntries(entries));
    }} /></label>;
    if (field.kind === "bytes") return <label key={id} htmlFor={id}>{label(field.name)}<input type="file" id={id} onChange={async (e) => {
      const file = e.target.files?.[0]; if (!file) { set(field, undefined); return; }
      if (file.size > 8 * 1024 * 1024) { e.target.setCustomValidity("Choose a file smaller than 8 MiB."); e.target.reportValidity(); return; }
      e.target.setCustomValidity("");
      const bytes = new Uint8Array(await file.arrayBuffer()); let binary = "";
      for (let i = 0; i < bytes.length; i += 8192) binary += String.fromCharCode(...bytes.subarray(i, i + 8192));
      set(field, btoa(binary));
    }} /></label>;
    if (field.kind === "bool" || field.options) return <label key={id} htmlFor={id}>{label(field.name)}<select id={id} value={current === undefined ? "" : String(current)} onChange={(e) => set(field, e.target.value === "" ? undefined : field.kind === "bool" ? e.target.value === "true" : e.target.value)}>
      <option value="">Use current setting / default</option>
      {field.kind === "bool" ? <><option value="true">Enabled</option><option value="false">Disabled</option></> : field.options?.map((option) => <option key={option} value={option}>{label(option.toLowerCase())}</option>)}
    </select></label>;
    const numeric = field.kind !== "string";
    return <label key={id} htmlFor={id}>{label(field.name)}<input id={id} type={/password|secret|token|hmacKey/i.test(field.name) ? "password" : numeric ? "number" : "text"} step={numeric ? "any" : undefined} value={String(current ?? "")} autoComplete="off" placeholder="Use default" onChange={(e) => set(field, e.target.value === "" ? undefined : numeric && !field.kind.includes("64") ? Number(e.target.value) : e.target.value)} /></label>;
  })}</div>;
}
