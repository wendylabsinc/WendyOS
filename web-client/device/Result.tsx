import { useState } from "react";
import { label } from "./rpc";
import { formatted, hasContent, visibleEntries } from "./presentation";
export default function Result({ value, depth = 0, field = "" }: { value: unknown; depth?: number; field?: string }) {
  const [expanded, setExpanded] = useState(false);
  if (!hasContent(value)) return depth ? null : <p className="empty-state">Nothing here yet.</p>;
  if (typeof value !== "object" || value === null) return <span className="device-value">{formatted(value, field)}</span>;
  if (Array.isArray(value)) {
    const items = value.filter(hasContent);
    if (items.every((item) => typeof item !== "object")) return <span className="device-value">{items.map((item) => formatted(item, field)).join(", ")}</span>;
    const objects = items.filter((item) => item && typeof item === "object" && !Array.isArray(item)) as Record<string, unknown>[];
    const keys = [...new Set(objects.flatMap((item) => visibleEntries(item).filter(([, v]) => typeof v !== "object").map(([key]) => key)))].sort((a, b) => { const priority = ["name", "description", "mountpoint", "state", "status", "healthy", "sizeBytes", "ipAddress", "kind"]; const rank = (key: string) => priority.includes(key) ? priority.indexOf(key) : 100; return rank(a) - rank(b); }).slice(0, 4);
    if (keys.length && objects.length === items.length) return <div className="result-table-wrap"><table className="result-table"><thead><tr>{keys.map((key) => <th key={key}>{label(key)}</th>)}<th className="sr-only">Details</th></tr></thead><tbody>{(expanded ? objects : objects.slice(0, 8)).map((item, i) => {
      const rest = Object.fromEntries(visibleEntries(item).filter(([key]) => !keys.includes(key)));
      return <tr key={i}>{keys.map((key) => <td key={key}>{hasContent(item[key]) ? formatted(item[key], key) : "—"}</td>)}<td>{hasContent(rest) && <details><summary>Details</summary><Result value={rest} depth={depth + 1} /></details>}</td></tr>;
    })}</tbody></table>{objects.length > 8 && <button className="quiet table-expand" onClick={() => setExpanded(!expanded)}>{expanded ? "Show fewer" : `Show all ${objects.length}`}</button>}</div>;
    return <div className="device-result-list">{items.map((item, i) => <Result key={i} value={item} depth={depth + 1} />)}</div>;
  }
  const entries = visibleEntries(value as Record<string, unknown>);
  if (entries.length === 1 && Array.isArray(entries[0][1])) return <Result value={entries[0][1]} depth={depth} field={entries[0][0]} />;
  const scalar = entries.filter(([, item]) => typeof item !== "object");
  const nested = entries.filter(([, item]) => typeof item === "object");
  return <>{scalar.length > 0 && <dl className="device-result">{scalar.map(([key, item]) => <div key={key}><dt>{label(key)}</dt><dd><Result value={item} depth={depth + 1} field={key} /></dd></div>)}</dl>}{nested.map(([key, item]) => <div className="result-group" key={key}>{Array.isArray(item) && depth === 0 ? <><h3>{label(key)}</h3><Result value={item} depth={depth + 1} field={key} /></> : <details><summary>{label(key)}{Array.isArray(item) ? ` · ${item.length}` : ""}</summary>{depth < 6 && <Result value={item} depth={depth + 1} field={key} />}</details>}</div>)}</>;
}
