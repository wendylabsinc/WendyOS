"use client";

import { useId, useState } from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { Activity } from "lucide-react";
import { formatMetricValue, type MetricSeries } from "@/lib/metric-charts";
import "./metric-charts.css";

function clock(time: number) {
  return new Date(time).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}

function MetricCard({ series, windowMs }: { series: MetricSeries; windowMs: number }) {
  const titleId = useId();
  const last = series.points.at(-1)?.time;
  const points = series.points.filter((point) => !last || !windowMs || point.time >= last - windowMs);
  const numeric = points.filter((point) => point.value !== null);
  const pending = series.kind === "rate" && numeric.length === 0;
  const xDomain: [number | "dataMin", number | "dataMax"] = points.length === 1 && last !== undefined ? [last - 15000, last + 15000] : ["dataMin", "dataMax"];
  return <article className="metric-chart-card" aria-labelledby={titleId}>
    <header className="metric-chart-heading">
      <div>
        <p className="metric-service-label">{series.serviceLabel}</p>
        <h3 id={titleId}>{series.title}</h3>
        {series.subtitle && <p className="metric-series-label">{series.subtitle}</p>}
      </div>
      <strong className="metric-current-value">{formatMetricValue(series.latest, series.unit)}</strong>
    </header>
    <div className="metric-chart-plot" aria-label={`${series.title}, ${series.serviceLabel}${series.subtitle ? `, ${series.subtitle}` : ""}`}>
      {numeric.length > 0 ? <ResponsiveContainer width="100%" height="100%" minWidth={0} initialDimension={{ width: 440, height: 180 }}>
        <LineChart data={points} margin={{ top: 10, right: 8, bottom: 0, left: 0 }} accessibilityLayer>
          <CartesianGrid vertical={false} stroke="var(--border)" strokeDasharray="3 5" />
          <XAxis dataKey="time" type="number" scale="time" domain={xDomain} tickFormatter={clock} minTickGap={45} tickLine={false} axisLine={false} tick={{ fill: "var(--muted-foreground)", fontSize: 10 }} />
          <YAxis width={72} domain={[(min: number) => Math.min(0, min), "auto"]} tickFormatter={(value: number) => formatMetricValue(value, series.unit)} tickCount={4} tickLine={false} axisLine={false} tick={{ fill: "var(--muted-foreground)", fontSize: 10 }} />
          <Tooltip content={({ active, payload, label }) => {
            const point = payload?.[0]?.payload as { time: number; value: number | null } | undefined;
            if (!active || !point) return null;
            return <div className="metric-chart-tooltip"><time>{clock(Number(label))}</time><strong>{formatMetricValue(point.value, series.unit)}</strong><span>{series.subtitle || series.title}</span></div>;
          }} cursor={{ stroke: "var(--muted-foreground)", strokeDasharray: "3 3" }} />
          <Line type="linear" dataKey="value" name={series.subtitle || series.title} stroke="var(--foreground)" strokeWidth={2} dot={numeric.length <= 2 ? { r: 3, strokeWidth: 0, fill: "var(--foreground)" } : false} activeDot={{ r: 4, stroke: "var(--card)", strokeWidth: 2 }} isAnimationActive={false} connectNulls={false} />
        </LineChart>
      </ResponsiveContainer> : <div className="metric-chart-pending"><Activity size={21} /><span>{pending ? "Collecting the next sample" : "Waiting for a numeric sample"}</span><small>{pending ? "CPU usage needs two measurements." : "Values will appear as the device reports them."}</small></div>}
    </div>
    <footer className="metric-chart-footer"><span>{series.kind === "rate" ? "100% = one CPU core" : series.kind === "distribution" ? "Average per sample" : series.kind === "counter" ? "Cumulative total" : ""}</span>{last !== undefined && <time dateTime={new Date(last).toISOString()}>Updated {clock(last)}</time>}</footer>
    <details className="metric-chart-details"><summary>Metric details</summary><dl><div><dt>Metric</dt><dd>{series.name}</dd></div><div><dt>Application</dt><dd>{series.service}</dd></div>{series.subtitle && <div><dt>Series</dt><dd>{series.subtitle}</dd></div>}</dl><details><summary>Raw sample</summary><pre>{JSON.stringify(series.detail, null, 2)}</pre></details></details>
  </article>;
}

export default function MetricCharts({ series, search = "" }: { series: MetricSeries[]; search?: string }) {
  const [windowMs, setWindowMs] = useState(5 * 60 * 1000);
  const query = search.trim().toLowerCase();
  const visible = series.filter((item) => [item.title, item.name, item.service, item.serviceLabel, item.subtitle].some((value) => value.toLowerCase().includes(query)));
  return <div className="metric-charts">
    <div className="metric-charts-toolbar"><p>History builds while this view is open.</p><label>Time range<select value={windowMs} onChange={(event) => setWindowMs(Number(event.target.value))} aria-label="Metric chart time range"><option value={300000}>Last 5 minutes</option><option value={900000}>Last 15 minutes</option><option value={0}>Available history</option></select></label></div>
    {visible.length ? <div className="metric-charts-grid">{visible.map((item) => <MetricCard key={item.key} series={item} windowMs={windowMs} />)}</div> : <div className="metric-charts-empty"><Activity size={24} /><h3>{series.length ? "No matching metrics" : "Waiting for metrics"}</h3><p>{series.length ? "Try a metric or application name." : "Charts appear when the device sends its first measurements."}</p></div>}
  </div>;
}
