import * as Tabs from "@radix-ui/react-tabs";
import {
  ColumnDef,
  flexRender,
  getCoreRowModel,
  useReactTable,
} from "@tanstack/react-table";
import {
  autoUpdate,
  flip,
  offset,
  shift,
  useFloating,
} from "@floating-ui/react";
import React, { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { createRoot } from "react-dom/client";
import "./styles.css";

type Manifest = {
  title: string;
  reports: ReportSummary[];
};

type ReportSummary = {
  id: string;
  label: string;
  path: string;
  latest_run_name: string;
  latest_run_status: string;
  run_count: number;
  measurement_count: number;
  engine?: string;
};

type Summary = {
  metadata: MetadataItem[];
  latest_run: { name: string; status: string; hardware: string };
  warnings: { level: string; message: string }[];
  measurement_count: number;
  context_status_counts: Record<string, number>;
};

type MetadataItem = {
  label: string;
  value: string;
};

type ThroughputResponse = {
  tables: ThroughputTable[];
};

type ThroughputTable = {
  id: string;
  title: string;
  profile: string;
  model: string;
  server_limit_label: string;
  context_label?: string;
  context_status: string;
  context_status_label: string;
  warning?: string;
  decode_shape?: string;
  prefill_shape?: string;
  shape_notes?: string[];
  rows: ThroughputRow[];
};

type ThroughputRow = {
  concurrency: number;
  baseline: boolean;
  decode: PhaseMetrics;
  prefill: PhaseMetrics;
  result: string;
  slo: string;
};

type PhaseMetrics = {
  available: boolean;
  measurement_id?: number;
  workload?: string;
  shape?: string;
  status?: string;
  tok_s?: string;
  per_user_tok_s?: string;
  ttft_mean_ms?: string;
  ttft_p99_ms?: string;
  tok_s_display?: string;
  per_user_tok_s_display?: string;
  ttft_mean_display?: string;
  ttft_p99_display?: string;
  ok: number;
  err: number;
  failure_label?: string;
  failure_reason?: string;
};

type CellDetail = {
  available: boolean;
  phase: string;
  mode: string;
  status: string;
  failure_label?: string;
  failure_reason?: string;
  run_id?: string;
  measurement_id: number;
  model?: string;
  profile?: string;
  workload?: string;
  context_label?: string;
  context_window?: number;
  concurrency?: number;
  samples_requested?: number;
  shape?: string;
  profile_config?: MetadataItem[];
  metrics?: MetadataItem[];
  serve_command?: string;
  benchmark_command?: string;
  engine_args?: string;
  serve_json?: string;
  env_json?: string;
};

type LoadState<T> =
  | { state: "loading" }
  | { state: "loaded"; value: T }
  | { state: "error"; message: string };

const api = async <T,>(path: string): Promise<T> => {
  const response = await fetch(path);
  if (!response.ok) {
    throw new Error(`${response.status} ${response.statusText}`);
  }
  return (await response.json()) as T;
};

function App() {
  const [manifest, setManifest] = useState<LoadState<Manifest>>({ state: "loading" });
  const [selected, setSelected] = useState("");

  useEffect(() => {
    api<Manifest>("/api/reports")
      .then((value) => {
        setManifest({ state: "loaded", value });
        setSelected((current) => current || value.reports[0]?.id || "");
      })
      .catch((error: Error) => setManifest({ state: "error", message: error.message }));
  }, []);

  if (manifest.state === "loading") {
    return <div className="app-message">Loading report artifacts...</div>;
  }
  if (manifest.state === "error") {
    return <div className="app-message error">Could not load reports: {manifest.message}</div>;
  }
  if (manifest.value.reports.length === 0) {
    return <div className="app-message">No report artifacts loaded.</div>;
  }

  const selectedReport = manifest.value.reports.find((report) => report.id === selected) ?? manifest.value.reports[0];

  return (
    <main className="app">
      <header className="app-head">
        <div>
          <h1>{manifest.value.title}</h1>
          <p>{manifest.value.reports.length} artifact{manifest.value.reports.length === 1 ? "" : "s"}</p>
        </div>
        <div className="run-chip">
          <span>{selectedReport.latest_run_status}</span>
          <strong>{selectedReport.latest_run_name || selectedReport.label}</strong>
        </div>
      </header>
      <EngineMismatchNote reports={manifest.value.reports} />
      <Tabs.Root value={selectedReport.id} onValueChange={setSelected} className="tabs-root">
        <Tabs.List className="tabs-list" aria-label="Report artifacts">
          {manifest.value.reports.map((report) => (
            <Tabs.Trigger className="tab-trigger" value={report.id} key={report.id}>
              <span>{report.label}</span>
              <small>{report.run_count} run{report.run_count === 1 ? "" : "s"} / {report.measurement_count} rows</small>
            </Tabs.Trigger>
          ))}
        </Tabs.List>
        {manifest.value.reports.map((report) => (
          <Tabs.Content value={report.id} key={report.id} className="tab-content">
            <ReportView report={report} />
          </Tabs.Content>
        ))}
      </Tabs.Root>
    </main>
  );
}

function EngineMismatchNote({ reports }: { reports: ReportSummary[] }) {
  const engines = Array.from(new Set(reports.map((report) => report.engine).filter(Boolean)));
  if (engines.length < 2) {
    return null;
  }
  return (
    <div className="warning compact">
      Engines differ across loaded reports ({engines.join(", ")}); compare tabs with care.
    </div>
  );
}

function ReportView({ report }: { report: ReportSummary }) {
  const [summary, setSummary] = useState<LoadState<Summary>>({ state: "loading" });
  const [throughput, setThroughput] = useState<LoadState<ThroughputResponse>>({ state: "loading" });

  useEffect(() => {
    setSummary({ state: "loading" });
    setThroughput({ state: "loading" });
    api<Summary>(`/api/reports/${report.id}/summary`)
      .then((value) => setSummary({ state: "loaded", value }))
      .catch((error: Error) => setSummary({ state: "error", message: error.message }));
    api<ThroughputResponse>(`/api/reports/${report.id}/throughput`)
      .then((value) => setThroughput({ state: "loaded", value }))
      .catch((error: Error) => setThroughput({ state: "error", message: error.message }));
  }, [report.id]);

  return (
    <section className="report-view">
      {summary.state === "loaded" && <SummaryStrip summary={summary.value} />}
      {summary.state === "error" && <div className="app-message error">Summary failed: {summary.message}</div>}
      {throughput.state === "loading" && <div className="app-message">Loading throughput tables...</div>}
      {throughput.state === "error" && <div className="app-message error">Throughput failed: {throughput.message}</div>}
      {throughput.state === "loaded" && (
        <div className="table-stack">
          {throughput.value.tables.map((table) => (
            <ThroughputTableView key={table.id} table={table} reportID={report.id} />
          ))}
        </div>
      )}
    </section>
  );
}

function SummaryStrip({ summary }: { summary: Summary }) {
  const visibleMeta = summary.metadata.slice(0, 8);
  return (
    <section className="summary-strip">
      {summary.warnings.map((warning) => (
        <div className="warning" key={warning.message}>{warning.message}</div>
      ))}
      <div className="meta-grid">
        {visibleMeta.map((item) => (
          <div className="meta-item" key={`${item.label}-${item.value}`}>
            <span>{item.label}</span>
            <strong>{item.value}</strong>
          </div>
        ))}
      </div>
    </section>
  );
}

function ThroughputTableView({ table, reportID }: { table: ThroughputTable; reportID: string }) {
  const enrichedRows = useMemo(() => withHeat(table.rows), [table.rows]);
  const showSLO = useMemo(() => table.rows.some((row) => isRealValue(row.slo)), [table.rows]);
  const columns = useMemo<ColumnDef<HeatRow>[]>(() => {
    const base: ColumnDef<HeatRow>[] = [
      { accessorKey: "concurrency", header: "Users", cell: (info) => <span className="num strong">{info.getValue<number>()}</span> },
      phaseColumn(reportID, "decode", "tokS", "Decode tok/s"),
      phaseColumn(reportID, "decode", "perUser", "Decode/user"),
      phaseColumn(reportID, "decode", "ttftAvg", "Decode TTFT avg"),
      phaseColumn(reportID, "decode", "ttftP99", "Decode TTFT p99"),
      phaseColumn(reportID, "prefill", "tokS", "Prefill tok/s"),
      phaseColumn(reportID, "prefill", "perUser", "Prefill/user"),
      phaseColumn(reportID, "prefill", "ttftAvg", "Prefill TTFT avg"),
      phaseColumn(reportID, "prefill", "ttftP99", "Prefill TTFT p99"),
      { accessorKey: "result", header: "OK / Err", cell: (info) => <span className="num">{info.row.original.result}</span> },
    ];
    if (showSLO) {
      base.push({ accessorKey: "slo", header: "SLO / goodput", cell: (info) => <span className="num">{info.row.original.slo || "-"}</span> });
    }
    return base;
  }, [reportID, showSLO]);
  const instance = useReactTable({ data: enrichedRows, columns, getCoreRowModel: getCoreRowModel() });

  return (
    <section className="table-panel">
      <div className="table-head">
        <div>
          <h2>{table.title}</h2>
          <div className="subline">
            <span>Server limit {table.server_limit_label}</span>
            {table.context_label && <span>Context {table.context_label}</span>}
            <span className={`status status-${table.context_status}`}>{table.context_status_label}</span>
          </div>
        </div>
        <div className="shape-grid">
          <Shape label="Decode" value={table.decode_shape} />
          <Shape label="Prefill" value={table.prefill_shape} />
        </div>
      </div>
      {table.warning && <div className="warning compact">{table.warning}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            {instance.getHeaderGroups().map((headerGroup) => (
              <tr key={headerGroup.id}>
                {headerGroup.headers.map((header) => (
                  <th key={header.id}>{flexRender(header.column.columnDef.header, header.getContext())}</th>
                ))}
              </tr>
            ))}
          </thead>
          <tbody>
            {instance.getRowModel().rows.map((row) => (
              <tr key={row.id}>
                {row.getVisibleCells().map((cell) => (
                  <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

type HeatRow = ThroughputRow & {
  heat: Record<string, string>;
};

type Phase = "decode" | "prefill";
type PhaseMetric = "tokS" | "perUser" | "ttftAvg" | "ttftP99";

function phaseColumn(reportID: string, phase: Phase, metric: PhaseMetric, header: string): ColumnDef<HeatRow> {
  return {
    id: `${phase}-${metric}`,
    header,
    cell: (info) => {
      const row = info.row.original;
      const value = displayValue(row[phase], metric);
      const heat = row.heat[`${phase}-${metric}`] ?? "heat-neutral";
      return (
        <MetricCell
          reportID={reportID}
          metric={row[phase]}
          value={value}
          heat={heat}
        />
      );
    },
  };
}

function MetricCell({ reportID, metric, value, heat }: { reportID: string; metric: PhaseMetrics; value: string; heat: string }) {
  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [detail, setDetail] = useState<LoadState<CellDetail> | null>(null);
  const { refs, floatingStyles } = useFloating({
    open,
    onOpenChange: setOpen,
    whileElementsMounted: autoUpdate,
    placement: "top",
    middleware: [offset(8), flip(), shift({ padding: 8 })],
  });
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const floatingRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open || !metric.measurement_id || detail) {
      return;
    }
    setDetail({ state: "loading" });
    api<CellDetail>(`/api/reports/${reportID}/measurements/${metric.measurement_id}`)
      .then((value) => setDetail({ state: "loaded", value }))
      .catch((error: Error) => setDetail({ state: "error", message: error.message }));
  }, [detail, metric.measurement_id, open, reportID]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const close = (event: MouseEvent) => {
      const target = event.target as Node;
      if (buttonRef.current?.contains(target)) {
        return;
      }
      if (floatingRef.current?.contains(target)) {
        return;
      }
      setOpen(false);
      setPinned(false);
    };
    const key = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        setPinned(false);
      }
    };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", key);
    };
  }, [open]);

  if (!metric.available) {
    return <span className="num muted">-</span>;
  }

  return (
    <>
      <button
        ref={(node) => {
          buttonRef.current = node;
          refs.setReference(node);
        }}
        className={`metric-trigger num ${heat}`}
        onMouseEnter={() => !pinned && setOpen(true)}
        onMouseLeave={() => !pinned && setOpen(false)}
        onFocus={() => !pinned && setOpen(true)}
        onBlur={() => !pinned && setOpen(false)}
        onClick={() => {
          setPinned((current) => !current);
          setOpen(true);
        }}
        title={metric.failure_reason || undefined}
      >
        {value || "-"}
      </button>
      {open && createPortal(
        <div
          ref={(node) => {
            floatingRef.current = node;
            refs.setFloating(node);
          }}
          style={floatingStyles}
          className="popover"
        >
          <DetailBody detail={detail} metric={metric} />
        </div>,
        document.body,
      )}
    </>
  );
}

function DetailBody({ detail, metric }: { detail: LoadState<CellDetail> | null; metric: PhaseMetrics }) {
  if (!detail || detail.state === "loading") {
    return <div className="detail-muted">Loading details...</div>;
  }
  if (detail.state === "error") {
    return <div className="detail-muted">Could not load details: {detail.message}</div>;
  }
  const value = detail.value;
  const pairs: MetadataItem[] = [
    { label: "Status", value: value.status || metric.status || "-" },
    { label: "Phase", value: value.phase || metric.workload || "-" },
    { label: "Model", value: value.model || "-" },
    { label: "Profile", value: value.profile || "-" },
    { label: "Workload", value: value.workload || "-" },
    { label: "Context", value: value.context_label || "-" },
    { label: "Users", value: String(value.concurrency || "-") },
    { label: "Shape", value: value.shape || metric.shape || "-" },
  ];
  return (
    <div className="detail">
      <div className="detail-grid">
        {pairs.map((item) => (
          <React.Fragment key={item.label}>
            <span>{item.label}</span>
            <strong>{item.value}</strong>
          </React.Fragment>
        ))}
      </div>
      {value.metrics && value.metrics.length > 0 && (
        <div className="detail-grid">
          {value.metrics.map((item) => (
            <React.Fragment key={item.label}>
              <span>{item.label}</span>
              <strong>{item.value}</strong>
            </React.Fragment>
          ))}
        </div>
      )}
      {(value.failure_reason || metric.failure_reason) && <CodeBlock label="Failure" value={value.failure_reason || metric.failure_reason || ""} />}
      {value.serve_command && <CodeBlock label="vLLM serve" value={value.serve_command} />}
      {value.benchmark_command && <CodeBlock label="Benchmark" value={value.benchmark_command} />}
      {value.engine_args && <CodeBlock label="Engine args" value={value.engine_args} />}
    </div>
  );
}

function CodeBlock({ label, value }: { label: string; value: string }) {
  return (
    <div className="code-block">
      <span>{label}</span>
      <code>{value}</code>
    </div>
  );
}

function Shape({ label, value }: { label: string; value?: string }) {
  return (
    <div className="shape">
      <span>{label}</span>
      <strong>{value || "-"}</strong>
    </div>
  );
}

function metricValue(metric: PhaseMetrics, field: PhaseMetric): string {
  if (!metric.available) {
    return "-";
  }
  switch (field) {
    case "tokS":
      return metric.tok_s || "-";
    case "perUser":
      return metric.per_user_tok_s || "-";
    case "ttftAvg":
      return metric.ttft_mean_ms || "-";
    case "ttftP99":
      return metric.ttft_p99_ms || "-";
  }
}

// displayValue prefers the server-formatted string; the raw value stays in
// the payload for heatmaps.
function displayValue(metric: PhaseMetrics, field: PhaseMetric): string {
  if (!metric.available) {
    return "-";
  }
  switch (field) {
    case "tokS":
      return metric.tok_s_display || metric.tok_s || "-";
    case "perUser":
      return metric.per_user_tok_s_display || metric.per_user_tok_s || "-";
    case "ttftAvg":
      return metric.ttft_mean_display || metric.ttft_mean_ms || "-";
    case "ttftP99":
      return metric.ttft_p99_display || metric.ttft_p99_ms || "-";
  }
}

function withHeat(rows: ThroughputRow[]): HeatRow[] {
  const out = rows.map((row) => ({ ...row, heat: {} }));
  for (const phase of ["decode", "prefill"] as Phase[]) {
    applyHeat(out, phase, "tokS", true);
    applyHeat(out, phase, "perUser", true);
    applyHeat(out, phase, "ttftAvg", false);
    applyHeat(out, phase, "ttftP99", false);
  }
  return out;
}

function applyHeat(rows: HeatRow[], phase: Phase, metric: PhaseMetric, higherBetter: boolean) {
  const values = rows
    .map((row) => parseNumber(metricValue(row[phase], metric)))
    .filter((value): value is number => value !== null);
  if (values.length === 0) {
    return;
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  for (const row of rows) {
    const value = parseNumber(metricValue(row[phase], metric));
    row.heat[`${phase}-${metric}`] = heatClass(value, min, max, higherBetter);
  }
}

function heatClass(value: number | null, min: number, max: number, higherBetter: boolean): string {
  if (value === null || max === min) {
    return "heat-neutral";
  }
  const raw = (value - min) / (max - min);
  const score = higherBetter ? raw : 1 - raw;
  return `heat-${Math.max(0, Math.min(5, Math.round(score * 5)))}`;
}

function parseNumber(value: string): number | null {
  const match = value.match(/-?\d+(\.\d+)?/);
  if (!match) {
    return null;
  }
  const parsed = Number(match[0]);
  return Number.isFinite(parsed) ? parsed : null;
}

function isRealValue(value: string): boolean {
  const trimmed = value.trim();
  return trimmed !== "" && trimmed !== "-";
}

createRoot(document.getElementById("root")!).render(<App />);
