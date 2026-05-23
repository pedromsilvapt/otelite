package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// handleUI serves the embedded single-page application.
func handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(uiHTML))
}

// handleAPIDBStats returns the SQLite database file size.
func handleAPIDBStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var size int64
	if dbFilePath != "" {
		if info, err := os.Stat(dbFilePath); err == nil {
			size = info.Size()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"size": size,
	})
}

// handleAPITracesList returns a summary list of traces, one row per trace_id.
func handleAPITracesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rows, err := db.Query(`
		SELECT
			t.trace_id,
			MIN(t.start_time)  AS first_start,
			MAX(t.end_time)    AS last_end,
			COUNT(*)           AS span_count,
			MAX(CASE WHEN t.status_code=2 THEN 2 WHEN t.status_code=1 THEN 1 ELSE 0 END) AS trace_status,
			(SELECT GROUP_CONCAT(svc, ',')
			 FROM (
			   SELECT service_name AS svc
			   FROM traces
			   WHERE trace_id = t.trace_id
			     AND service_name IS NOT NULL AND service_name != ''
			   GROUP BY service_name
			   ORDER BY MIN(start_time) ASC
			 )
			) AS services,
			(SELECT span_name FROM traces
			 WHERE trace_id = t.trace_id
			   AND (parent_span_id = '' OR parent_span_id IS NULL)
			 ORDER BY start_time ASC LIMIT 1) AS root_span_name
		FROM traces t
		GROUP BY t.trace_id
		ORDER BY first_start DESC
		LIMIT 200
	`)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type TraceEntry struct {
		TraceID      string `json:"trace_id"`
		FirstStart   int64  `json:"first_start"`
		LastEnd      int64  `json:"last_end"`
		SpanCount    int    `json:"span_count"`
		StatusCode   int    `json:"status_code"`
		Services     string `json:"services"`
		RootSpanName string `json:"root_span_name"`
	}

	var traces []TraceEntry
	for rows.Next() {
		var e TraceEntry
		var rootSpanName, services sql.NullString
		if err := rows.Scan(&e.TraceID, &e.FirstStart, &e.LastEnd, &e.SpanCount, &e.StatusCode, &services, &rootSpanName); err != nil {
			continue
		}
		if services.Valid {
			e.Services = services.String
		}
		if rootSpanName.Valid {
			e.RootSpanName = rootSpanName.String
		}
		traces = append(traces, e)
	}
	if traces == nil {
		traces = []TraceEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"traces": traces,
		"count":  len(traces),
	})
}

// handleAPITrace returns all spans for a single trace_id extracted from the URL path.
func handleAPITrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := strings.TrimPrefix(r.URL.Path, "/api/traces/")
	if traceID == "" {
		http.Error(w, "missing trace id", http.StatusBadRequest)
		return
	}

	rows, err := db.Query(`
		SELECT id, trace_id, span_id, parent_span_id, service_name, activity_source, span_name,
		       kind, start_time, end_time, status_code, attributes
		FROM traces
		WHERE trace_id = ?
		ORDER BY start_time ASC
	`, traceID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type SpanEntry struct {
		ID             int             `json:"id"`
		TraceID        string          `json:"trace_id"`
		SpanID         string          `json:"span_id"`
		ParentSpanID   string          `json:"parent_span_id"`
		ServiceName    string          `json:"service_name"`
		ActivitySource string          `json:"activity_source"`
		SpanName       string          `json:"span_name"`
		Kind           int             `json:"kind"`
		StartTime      int64           `json:"start_time"`
		EndTime        int64           `json:"end_time"`
		StatusCode     int             `json:"status_code"`
		Attributes     json.RawMessage `json:"attributes,omitempty"`
	}

	var spans []SpanEntry
	for rows.Next() {
		var s SpanEntry
		var attrs sql.NullString
		var activitySource sql.NullString
		if err := rows.Scan(&s.ID, &s.TraceID, &s.SpanID, &s.ParentSpanID,
			&s.ServiceName, &activitySource, &s.SpanName, &s.Kind,
			&s.StartTime, &s.EndTime, &s.StatusCode, &attrs); err != nil {
			continue
		}
		if activitySource.Valid {
			s.ActivitySource = activitySource.String
		}
		if attrs.Valid {
			s.Attributes = json.RawMessage(attrs.String)
		}
		spans = append(spans, s)
	}
	if spans == nil {
		spans = []SpanEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trace_id": traceID,
		"spans":    spans,
		"count":    len(spans),
	})
}

// handleAPILogs returns log records, optionally filtered by trace_id query param.
func handleAPILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	traceID := r.URL.Query().Get("trace_id")

	var (
		rows *sql.Rows
		err  error
	)
	const q = `
		SELECT id, timestamp, trace_id, span_id, service_name,
		       severity_number, severity_text, body, log_timestamp
		FROM logs
		%s
		ORDER BY log_timestamp DESC
		LIMIT 500
	`
	if traceID != "" {
		rows, err = db.Query(strings.Replace(q, "%s", "WHERE trace_id = ?", 1), traceID)
	} else {
		rows, err = db.Query(strings.Replace(q, "%s", "", 1))
	}
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type LogEntry struct {
		ID             int       `json:"id"`
		Timestamp      time.Time `json:"timestamp"`
		TraceID        string    `json:"trace_id"`
		SpanID         string    `json:"span_id"`
		ServiceName    string    `json:"service_name"`
		SeverityNumber int       `json:"severity_number"`
		SeverityText   string    `json:"severity_text"`
		Body           string    `json:"body"`
		LogTimestamp   int64     `json:"log_timestamp"`
	}

	var logs []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.TraceID, &l.SpanID,
			&l.ServiceName, &l.SeverityNumber, &l.SeverityText,
			&l.Body, &l.LogTimestamp); err != nil {
			continue
		}
		logs = append(logs, l)
	}
	if logs == nil {
		logs = []LogEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":  logs,
		"count": len(logs),
	})
}

// uiHTML is the complete single-page application served at /ui.
const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>otelite</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:ital,wght@0,300;0,400;0,500;0,600;1,400&display=swap" rel="stylesheet">
<style>
*{box-sizing:border-box;margin:0;padding:0}

:root{
  --bg:#0d1117;
  --surface:#161b22;
  --surface2:#1c2128;
  --border:#21262d;
  --border2:#30363d;
  --text:#e6edf3;
  --muted:#7d8590;
  --accent:#58a6ff;
  --green:#3fb950;
  --yellow:#d29922;
  --red:#f85149;
  --orange:#fb8f44;
  --font:'JetBrains Mono','Courier New',monospace;
}

body{
  background:var(--bg);
  color:var(--text);
  font-family:var(--font);
  font-size:13px;
  line-height:1.6;
  min-height:100vh;
}

/* ── NAV ──────────────────────────────────────── */
nav{
  display:flex;
  align-items:center;
  padding:0 24px;
  height:48px;
  border-bottom:1px solid var(--border);
  background:var(--surface);
  position:sticky;
  top:0;
  z-index:100;
}
.nav-logo{
  font-size:15px;font-weight:600;color:var(--text);
  text-decoration:none;letter-spacing:-0.5px;margin-right:32px;
}
.nav-logo span{color:var(--accent)}
.nav-link{
  display:inline-block;padding:0 14px;height:48px;line-height:48px;
  color:var(--muted);text-decoration:none;font-size:13px;
  transition:color .15s;border-bottom:2px solid transparent;margin-bottom:-1px;
}
.nav-link:hover{color:var(--text)}
.nav-link.active{color:var(--accent);border-bottom-color:var(--accent)}
.nav-spacer{flex:1}
.btn-danger{
  background:transparent;border:1px solid var(--border2);
  color:var(--muted);font-family:var(--font);font-size:12px;
  padding:5px 12px;border-radius:6px;cursor:pointer;
  display:inline-flex;align-items:center;gap:6px;
  transition:color .15s,border-color .15s,background .15s;
}
.btn-danger:hover{color:var(--red);border-color:var(--red);background:rgba(248,81,73,.08)}

.db-size{font-size:11px;color:var(--muted);margin-right:10px;white-space:nowrap}

/* ── PAGINATION ──────────────────────────────── */
.pager{display:flex;align-items:center;gap:6px;padding:12px 0;justify-content:center}
.pager-btn{
  background:var(--surface2);border:1px solid var(--border2);
  color:var(--muted);font-family:var(--font);font-size:12px;
  padding:4px 10px;border-radius:6px;cursor:pointer;min-width:32px;
  transition:background .15s,border-color .15s,color .15s;
}
.pager-btn:hover{background:var(--border2);color:var(--text)}
.pager-btn.active{background:var(--accent);border-color:var(--accent);color:#0d1117}
.pager-info{font-size:11px;color:var(--muted);margin-right:6px}

/* ── PAGE SHELL ───────────────────────────────── */
.page{padding:24px;animation:fadeIn .18s ease}
@keyframes fadeIn{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:translateY(0)}}

.page-header{margin-bottom:16px;display:flex;align-items:center;gap:10px}
.page-title{font-size:15px;font-weight:600}

.badge{
  font-size:11px;padding:2px 8px;
  background:var(--surface2);border:1px solid var(--border2);
  border-radius:20px;color:var(--muted);
}

/* ── FILTER BAR ───────────────────────────────── */
.filter-bar{display:flex;align-items:center;gap:10px;margin-bottom:16px}
.filter-input{
  background:var(--surface);border:1px solid var(--border2);
  color:var(--text);font-family:var(--font);font-size:12px;
  padding:6px 12px;border-radius:6px;outline:none;width:340px;
  transition:border-color .15s;
}
.filter-input:focus{border-color:var(--accent)}
.filter-input::placeholder{color:var(--muted)}

.btn{
  background:var(--surface2);border:1px solid var(--border2);
  color:var(--text);font-family:var(--font);font-size:12px;
  padding:6px 14px;border-radius:6px;cursor:pointer;
  transition:background .15s,border-color .15s;
}
.btn:hover{background:var(--border2);border-color:var(--muted)}

.filter-section{margin-bottom:10px;display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.filter-label{font-size:10px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);white-space:nowrap;min-width:52px}
.chip-group{display:flex;gap:6px;flex-wrap:wrap}
.chip{
  background:var(--surface2);border:1px solid var(--border2);
  color:var(--muted);font-family:var(--font);font-size:11px;
  padding:3px 10px;border-radius:20px;cursor:pointer;
  transition:color .15s,border-color .15s,background .15s;
  user-select:none;
}
.chip:hover{border-color:var(--muted);color:var(--text)}
.chip.active{border-color:var(--chip-clr,var(--accent));color:var(--chip-clr,var(--accent));background:var(--surface)}

/* ── DATA TABLE ───────────────────────────────── */
.data-table{
  width:100%;border-collapse:collapse;
  border:1px solid var(--border);border-radius:8px;overflow:hidden;
}
.data-table th{
  background:var(--surface);color:var(--muted);font-weight:500;
  font-size:11px;text-transform:uppercase;letter-spacing:.06em;
  padding:10px 14px;text-align:left;border-bottom:1px solid var(--border);
}
.data-table td{
  padding:10px 14px;border-bottom:1px solid var(--border);
  vertical-align:middle;
}
.data-table tr:last-child td{border-bottom:none}
.data-table tbody tr:hover td{background:var(--surface2);cursor:pointer}
.data-table .empty td{text-align:center;color:var(--muted);padding:48px;cursor:default}
.data-table .empty td:hover{background:transparent}

.trace-id{font-size:12px;color:var(--accent)}
.mono{font-family:var(--font)}
.muted{color:var(--muted)}
.small{font-size:11px}

/* ── SEVERITY BADGES ─────────────────────────── */
.sev{font-size:11px;font-weight:600;padding:2px 7px;border-radius:4px;display:inline-block}
.sev-trace,.sev-debug{background:#21262d;color:var(--muted)}
.sev-info{background:#1a3a1f;color:var(--green)}
.sev-warn{background:#2d2000;color:var(--yellow)}
.sev-error,.sev-fatal{background:#2d0f0f;color:var(--red)}

/* ── BACK BUTTON ─────────────────────────────── */
.back-btn{
  display:inline-flex;align-items:center;gap:6px;
  color:var(--muted);font-size:12px;padding:4px 0;
  margin-bottom:16px;cursor:pointer;
  background:none;border:none;font-family:var(--font);
  transition:color .15s;
}
.back-btn:hover{color:var(--text)}

/* ── DETAIL HEADER ───────────────────────────── */
.detail-header{
  background:var(--surface);border:1px solid var(--border);
  border-radius:8px;padding:20px;margin-bottom:16px;
}
.detail-trace-id{
  font-size:14px;font-weight:600;color:var(--accent);
  letter-spacing:.02em;word-break:break-all;
}
.detail-meta{margin-top:14px;display:flex;gap:28px;flex-wrap:wrap}
.detail-meta-item{display:flex;flex-direction:column;gap:3px}
.detail-meta-label{font-size:10px;text-transform:uppercase;letter-spacing:.1em;color:var(--muted)}
.detail-meta-value{font-size:13px;font-weight:500}

/* ── LOGS LINK BUTTON ────────────────────────── */
.logs-link{
  display:inline-flex;align-items:center;gap:8px;
  color:var(--accent);font-size:12px;padding:7px 14px;
  background:var(--surface);border:1px solid var(--border2);
  border-radius:6px;cursor:pointer;margin-bottom:16px;
  font-family:var(--font);text-decoration:none;
  transition:border-color .15s,background .15s;
}
.logs-link:hover{border-color:var(--accent);background:var(--surface2)}

/* ── SECTIONS ────────────────────────────────── */
.section{
  background:var(--surface);border:1px solid var(--border);
  border-radius:8px;margin-bottom:16px;overflow:hidden;
}
.section-header{
  padding:10px 20px;border-bottom:1px solid var(--border);
  font-size:11px;font-weight:600;text-transform:uppercase;
  letter-spacing:.1em;color:var(--muted);
  display:flex;align-items:center;gap:10px;
}
.section-body{padding:0}

/* ── ATTRIBUTES TABLE ────────────────────────── */
.attr-table{width:100%;border-collapse:collapse}
.attr-table td{padding:8px 20px;border-bottom:1px solid var(--border);font-size:12px;vertical-align:top}
.attr-table tr:last-child td{border-bottom:none}
.attr-key{color:var(--muted);width:240px;min-width:160px}
.attr-val{color:var(--text);word-break:break-all}

/* ── WATERFALL ───────────────────────────────── */
.waterfall-wrap{overflow-x:auto}
.wf-table{width:100%;border-collapse:collapse;min-width:760px}
.wf-table th{
  padding:8px 14px;font-size:10px;font-weight:600;
  text-transform:uppercase;letter-spacing:.06em;
  color:var(--muted);background:var(--surface2);
  border-bottom:1px solid var(--border);text-align:left;
}
.wf-table th.th-timeline{padding:0;position:relative}
.wf-table td{
  padding:5px 14px;border-bottom:1px solid var(--border);
  vertical-align:middle;white-space:nowrap;
}
.wf-table tr:last-child td{border-bottom:none}
.wf-table tr:hover td{background:rgba(255,255,255,.02)}
.wf-col-name{min-width:240px;max-width:300px}
.wf-col-svc{min-width:90px;font-size:11px}
.wf-col-dur{min-width:80px;text-align:right;font-size:11px;color:var(--muted)}
.wf-col-bar{width:100%;padding:5px 0}

.span-name-wrap{display:flex;align-items:center;gap:7px;overflow:hidden}
.span-dot{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.span-name-text{overflow:hidden;text-overflow:ellipsis;font-size:12px}

/* time axis */
.time-axis{
  position:relative;height:26px;
  background:var(--surface2);border-bottom:1px solid var(--border);
}
.t-tick{
  position:absolute;top:0;font-size:10px;color:var(--muted);
  transform:translateX(-50%);padding:5px 4px;white-space:nowrap;
}
.t-tick:first-child{transform:none}
.t-tick:last-child{transform:translateX(-100%)}

/* bar track */
.bar-track{position:relative;height:20px}
.bar{
  position:absolute;top:3px;height:14px;border-radius:3px;
  min-width:2px;opacity:.8;transition:opacity .1s;
}
.bar:hover{opacity:1}

/* ── STATE ───────────────────────────────────── */
.loading{color:var(--muted);padding:64px;text-align:center;font-size:14px}
.empty-state{color:var(--muted);padding:64px;text-align:center;font-size:14px}

.wf-span-row{cursor:pointer}
.wf-span-row:hover td{background:rgba(255,255,255,.025)}
.wf-span-row.span-row-open>td{background:var(--surface2)}
.span-detail-row>td{padding:0;background:var(--surface2);border-bottom:1px solid var(--border)}
.span-detail-cell{padding:10px 20px 10px 48px}
.span-detail-cell .attr-table td{padding:5px 16px;font-size:11px}
.no-attrs{color:var(--muted);font-size:12px;padding:12px 20px;display:block}

.log-msg{max-width:640px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.log-msg.expanded{white-space:pre-wrap;overflow:visible;max-width:none;word-break:break-word}

/* ── SCROLLBAR ───────────────────────────────── */
::-webkit-scrollbar{width:7px;height:7px}
::-webkit-scrollbar-track{background:var(--bg)}
::-webkit-scrollbar-thumb{background:var(--border2);border-radius:4px}
::-webkit-scrollbar-thumb:hover{background:var(--muted)}
</style>
</head>
<body>

<nav>
  <a class="nav-logo" href="#/traces">otel<span>ite</span></a>
  <a class="nav-link" id="nav-traces" href="#/traces">Traces</a>
  <a class="nav-link" id="nav-logs"   href="#/logs">Logs</a>
  <span class="nav-spacer"></span>
  <span class="db-size" id="db-size-label"></span>
  <button class="btn-danger" onclick="deleteAllData()" title="Delete all traces and logs">
    <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" xmlns="http://www.w3.org/2000/svg">
      <path d="M6.5 1h3a.5.5 0 0 1 .5.5v1H6v-1a.5.5 0 0 1 .5-.5zM11 2.5v-1A1.5 1.5 0 0 0 9.5 0h-3A1.5 1.5 0 0 0 5 1.5v1H2.506a.58.58 0 0 0-.01 0H1.5a.5.5 0 0 0 0 1h.538l.853 10.66A2 2 0 0 0 4.885 16h6.23a2 2 0 0 0 1.994-1.84l.853-10.66H14.5a.5.5 0 0 0 0-1h-.995a.59.59 0 0 0-.01 0H11zm1.958 1-.846 10.58a1 1 0 0 1-.997.92h-6.23a1 1 0 0 1-.997-.92L3.042 3.5h9.916z"/>
    </svg>
    Clear data
  </button>
</nav>

<div id="app"></div>

<script>
// ── Helpers ────────────────────────────────────────────────────────────────

function esc(s){
  return String(s==null?'':s)
    .replace(/&/g,'&amp;').replace(/</g,'&lt;')
    .replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function fmtDur(ns){
  if(!ns||ns<=0)return'—';
  if(ns<1000)return ns+'ns';
  const us=ns/1000;
  if(us<1000)return us.toFixed(1)+'µs';
  const ms=us/1000;
  if(ms<1000)return ms.toFixed(2)+'ms';
  return(ms/1000).toFixed(3)+'s';
}

function fmtNano(ns){
  if(!ns||ns<=0)return'—';
  return new Date(ns/1e6).toLocaleString();
}

function fmtTs(ts){
  if(!ts)return'—';
  return new Date(ts).toLocaleString();
}

function shortId(id){
  if(!id)return'—';
  return id.length>16?id.slice(0,16)+'…':id;
}

const PALETTE=['#58a6ff','#3fb950','#d29922','#fb8f44','#a5a5ff','#79c0ff','#56d364','#e3b341','#f78166','#bc8cff'];
const _svcClr={};let _ci=0;
function svcColor(s){if(!_svcClr[s])_svcClr[s]=PALETTE[_ci++%PALETTE.length];return _svcClr[s];}

// Returns the display label for a span's source column.
// If ActivitySource.Name starts with ServiceName, show only ActivitySource.Name
// (it already carries enough context); otherwise prefix with ServiceName.
function spanSvcLabel(svc,src){
  if(!src)return esc(svc||'—');
  if(svc&&src.startsWith(svc))return esc(src);
  return esc((svc?svc+' ':'')+src);
}

function sevClass(n){
  if(n>=21)return'sev-fatal';if(n>=17)return'sev-error';
  if(n>=13)return'sev-warn';if(n>=9)return'sev-info';
  if(n>=5)return'sev-debug';return'sev-trace';
}
function sevLabel(n,t){
  if(t)return t;
  if(n>=21)return'FATAL';if(n>=17)return'ERROR';
  if(n>=13)return'WARN';if(n>=9)return'INFO';
  if(n>=5)return'DEBUG';return'TRACE';
}
function statusCls(c){return c===2?'color:var(--red)':c===1?'color:var(--green)':'color:var(--muted)'}
function statusLbl(c){return c===2?'ERROR':c===1?'OK':'UNSET'}

function fmtBytes(b){
  if(!b||b<=0)return'';
  if(b<1024)return b+' B';
  if(b<1048576)return(b/1024).toFixed(1)+' KB';
  if(b<1073741824)return(b/1048576).toFixed(1)+' MB';
  return(b/1073741824).toFixed(2)+' GB';
}

function renderPager(id,page,total,pageSize,fnName){
  const el=document.getElementById(id);if(!el)return;
  const totalPages=Math.ceil(total/pageSize);
  if(totalPages<=1){el.innerHTML='';return;}
  const s=Math.max(1,page-2),e=Math.min(totalPages,page+2);
  const from=(page-1)*pageSize+1,to=Math.min(page*pageSize,total);
  let h='<div class="pager"><span class="pager-info">'+from+'–'+to+' of '+total+'</span>';
  if(page>1)h+='<button class="pager-btn" onclick="'+fnName+'('+(page-1)+')">←</button>';
  for(let i=s;i<=e;i++)h+='<button class="pager-btn'+(i===page?' active':'')+'" onclick="'+fnName+'('+i+')">'+i+'</button>';
  if(page<totalPages)h+='<button class="pager-btn" onclick="'+fnName+'('+(page+1)+')">→</button>';
  h+='</div>';
  el.innerHTML=h;
}

// ── Router ─────────────────────────────────────────────────────────────────

function go(h){location.hash=h}

window.addEventListener('hashchange',route);
window.addEventListener('load',route);
window.addEventListener('load',async function(){
  try{
    const d=await apiFetch('/api/db-stats');
    const el=document.getElementById('db-size-label');
    if(el){const s=fmtBytes(d.size||0);if(s)el.textContent=s;}
  }catch(_){}
});

function route(){
  const h=location.hash.replace(/^#/,'')||'/traces';
  const parts=h.split('/').filter(Boolean);
  document.querySelectorAll('.nav-link').forEach(l=>l.classList.remove('active'));
  if(parts[0]==='trace'&&parts[1]){
    document.getElementById('nav-traces').classList.add('active');
    renderTraceDetail(decodeURIComponent(parts[1]));
  }else if(parts[0]==='logs'){
    document.getElementById('nav-logs').classList.add('active');
    renderLogs(parts[1]?decodeURIComponent(parts[1]):'');
  }else{
    document.getElementById('nav-traces').classList.add('active');
    renderTraces();
  }
}

async function apiFetch(url){
  const r=await fetch(url);
  if(!r.ok)throw new Error('HTTP '+r.status);
  return r.json();
}

async function deleteAllData(){
  if(!confirm('Delete all traces and logs? This cannot be undone.'))return;
  try{
    const r=await fetch('/api/data',{method:'DELETE'});
    if(!r.ok)throw new Error('HTTP '+r.status);
    route();
  }catch(e){
    alert('Failed to clear data: '+e.message);
  }
}

// ── Global filter state ────────────────────────────────────────────────────

const _ls={data:[],traceId:'',text:'',services:new Set(),levels:new Set(),page:1,pageSize:50};
const _ts={data:[],services:new Set(),statuses:new Set(),page:1,pageSize:50};
const SEV_LABELS=['TRACE','DEBUG','INFO','WARN','ERROR','FATAL'];
const SEV_COLORS={TRACE:'var(--muted)',DEBUG:'var(--muted)',INFO:'var(--green)',WARN:'var(--yellow)',ERROR:'var(--red)',FATAL:'var(--red)'};
function sevRange(label){return{TRACE:[1,4],DEBUG:[5,8],INFO:[9,12],WARN:[13,16],ERROR:[17,20],FATAL:[21,99]}[label]||[0,99];}

// ── Page: Traces List ──────────────────────────────────────────────────────

async function renderTraces(){
  const app=document.getElementById('app');
  app.innerHTML='<div class="loading">Loading traces…</div>';
  let data;
  try{data=await apiFetch('/api/traces')}
  catch(e){app.innerHTML=err('Failed to load traces: '+e.message);return}

  _ts.data=data.traces||[];
  _ts.services=new Set();
  _ts.statuses=new Set();
  _ts.page=1;

  const allSvcs=new Set();
  _ts.data.forEach(t=>(t.services||'').split(',').filter(Boolean).forEach(s=>allSvcs.add(s)));
  const svcList=[...allSvcs].sort();

  const allStatCodes=new Set(_ts.data.map(t=>t.status_code||0));
  const STATUS_OPTS=[{code:2,label:'ERROR',clr:'var(--red)'},{code:1,label:'OK',clr:'var(--green)'},{code:0,label:'UNSET',clr:'var(--muted)'}];
  const statOpts=STATUS_OPTS.filter(s=>allStatCodes.has(s.code));

  const svcFilterHtml=svcList.length>1
    ?'<div class="filter-section"><span class="filter-label">Service</span><div class="chip-group" id="t-svc-chips">'+
      svcList.map(s=>'<button class="chip" data-svc="'+esc(s)+'" onclick="toggleTraceService(\''+esc(s)+'\')" style="--chip-clr:'+svcColor(s)+'">'+esc(s)+'</button>').join('')+
      '</div></div>':''
  ;
  const statFilterHtml=statOpts.length>1
    ?'<div class="filter-section"><span class="filter-label">Status</span><div class="chip-group" id="t-status-chips">'+
      statOpts.map(s=>'<button class="chip" data-status="'+s.code+'" onclick="toggleTraceStatus('+s.code+')" style="--chip-clr:'+s.clr+'">'+s.label+'</button>').join('')+
      '</div></div>':''
  ;

  app.innerHTML=` + "`" + `
  <div class="page">
    <div class="page-header">
      <span class="page-title">Traces</span>
      <span class="badge" id="traces-count">${_ts.data.length}</span>
    </div>
    ${svcFilterHtml}
    ${statFilterHtml}
    <table class="data-table">
      <thead><tr>
        <th>Trace ID</th><th>Status</th><th>Root Span</th><th>Services</th>
        <th>Start</th><th>Duration</th><th>Spans</th>
      </tr></thead>
      <tbody id="traces-tbody"></tbody>
    </table>
    <div id="traces-pager"></div>
  </div>` + "`" + `;

  renderTracesBody();
}

function toggleTraceService(s){
  const btn=document.querySelector('#t-svc-chips [data-svc="'+s+'"]');
  if(_ts.services.has(s)){_ts.services.delete(s);btn&&btn.classList.remove('active');}
  else{_ts.services.add(s);btn&&btn.classList.add('active');}
  _ts.page=1;renderTracesBody();
}

function toggleTraceStatus(code){
  const btn=document.querySelector('[data-status="'+code+'"]');
  if(_ts.statuses.has(code)){_ts.statuses.delete(code);btn&&btn.classList.remove('active');}
  else{_ts.statuses.add(code);btn&&btn.classList.add('active');}
  _ts.page=1;renderTracesBody();
}

function setTracesPage(p){_ts.page=p;renderTracesBody();}

function renderTracesBody(){
  let list=_ts.data;
  if(_ts.services.size>0){
    list=list.filter(t=>{
      const svcs=(t.services||'').split(',').filter(Boolean);
      return svcs.some(s=>_ts.services.has(s));
    });
  }
  if(_ts.statuses.size>0){list=list.filter(t=>_ts.statuses.has(t.status_code||0));}
  const total=list.length;
  const countEl=document.getElementById('traces-count');
  if(countEl)countEl.textContent=total;
  const tbody=document.getElementById('traces-tbody');
  if(!tbody)return;
  if(!total){
    tbody.innerHTML='<tr class="empty"><td colspan="7">No traces found</td></tr>';
    renderPager('traces-pager',1,0,_ts.pageSize,'setTracesPage');
    return;
  }
  const start=(_ts.page-1)*_ts.pageSize;
  const page=list.slice(start,start+_ts.pageSize);
  tbody.innerHTML=page.map(t=>{
    const dur=(t.last_end&&t.first_start)?t.last_end-t.first_start:0;
    const svcs=(t.services||'').split(',').filter(Boolean);
    const sc=t.status_code||0;
    return` + "`" + `<tr onclick="go('/trace/${t.trace_id}')">
      <td><span class="trace-id">${shortId(t.trace_id)}</span></td>
      <td><span style="font-size:11px;font-weight:600;${statusCls(sc)}">${statusLbl(sc)}</span></td>
      <td>${esc(t.root_span_name)||'<span class="muted">—</span>'}</td>
      <td>${svcs.map(s=>'<span style="color:'+svcColor(s)+'">'+esc(s)+'</span>').join(' ')}</td>
      <td class="muted small">${fmtNano(t.first_start)}</td>
      <td class="mono">${fmtDur(dur)}</td>
      <td class="muted">${t.span_count}</td>
    </tr>` + "`" + `;
  }).join('');
  renderPager('traces-pager',_ts.page,total,_ts.pageSize,'setTracesPage');
}

// ── Page: Logs List ────────────────────────────────────────────────────────

async function renderLogs(filterTraceId){
  const app=document.getElementById('app');
  app.innerHTML='<div class="loading">Loading logs…</div>';

  _ls.traceId=filterTraceId||'';
  _ls.text='';
  _ls.services=new Set();
  _ls.levels=new Set();
  _ls.page=1;

  const url='/api/logs'+(filterTraceId?'?trace_id='+encodeURIComponent(filterTraceId):'');
  let data;
  try{data=await apiFetch(url)}
  catch(e){app.innerHTML=err('Failed to load logs: '+e.message);return}

  _ls.data=data.logs||[];

  const allSvcs=new Set(_ls.data.map(l=>l.service_name).filter(Boolean));
  const svcList=[...allSvcs].sort();

  const usedLevels=SEV_LABELS.filter(lbl=>{
    const [lo,hi]=sevRange(lbl);
    return _ls.data.some(l=>(l.severity_number||0)>=lo&&(l.severity_number||0)<=hi);
  });

  app.innerHTML=` + "`" + `
  <div class="page">
    <div class="page-header">
      <span class="page-title">Logs</span>
      <span class="badge" id="logs-count">${_ls.data.length}</span>
    </div>
    <div class="filter-section">
      <span class="filter-label">Search</span>
      <input class="filter-input" id="log-search" placeholder="Search messages…" oninput="onLogSearch(this.value)" value="">
      ${filterTraceId?'<span style="font-size:12px;color:var(--muted)">trace: <span style="color:var(--accent)">'+esc(filterTraceId.slice(0,20))+'</span></span><button class="btn" style="padding:4px 10px" onclick="go(\'/logs\')">Clear</button>':''}
    </div>
    ${svcList.length>1?` + "`" + `
    <div class="filter-section">
      <span class="filter-label">Service</span>
      <div class="chip-group" id="l-svc-chips">
        ${svcList.map(s=>'<button class="chip" data-log-svc="'+esc(s)+'" onclick="toggleLogService(\''+esc(s)+'\')" style="--chip-clr:'+svcColor(s)+'">'+esc(s)+'</button>').join('')}
      </div>
    </div>` + "`" + `:''}
    ${usedLevels.length>1?` + "`" + `
    <div class="filter-section" style="margin-bottom:16px">
      <span class="filter-label">Level</span>
      <div class="chip-group" id="l-lvl-chips">
        ${usedLevels.map(lbl=>'<button class="chip" data-log-lbl="'+lbl+'" onclick="toggleLogLevel(\''+lbl+'\')" style="--chip-clr:'+(SEV_COLORS[lbl]||'var(--accent)')+'">'+lbl+'</button>').join('')}
      </div>
    </div>` + "`" + `:''}
    <table class="data-table">
      <thead><tr>
        <th>Time</th><th>Severity</th><th>Service</th><th>Trace ID</th><th>Message</th>
      </tr></thead>
      <tbody id="logs-tbody"></tbody>
    </table>
    <div id="logs-pager"></div>
  </div>` + "`" + `;

  renderLogsBody();
}

function onLogSearch(v){_ls.text=v.toLowerCase();_ls.page=1;renderLogsBody();}

function toggleLogService(s){
  const btn=document.querySelector('[data-log-svc="'+s+'"]');
  if(_ls.services.has(s)){_ls.services.delete(s);btn&&btn.classList.remove('active');}
  else{_ls.services.add(s);btn&&btn.classList.add('active');}
  _ls.page=1;renderLogsBody();
}

function toggleLogLevel(lbl){
  const btn=document.querySelector('[data-log-lbl="'+lbl+'"]');
  if(_ls.levels.has(lbl)){_ls.levels.delete(lbl);btn&&btn.classList.remove('active');}
  else{_ls.levels.add(lbl);btn&&btn.classList.add('active');}
  _ls.page=1;renderLogsBody();
}

function setLogsPage(p){_ls.page=p;renderLogsBody();}

function renderLogsBody(){
  let list=_ls.data;
  if(_ls.text){list=list.filter(l=>(l.body||'').toLowerCase().includes(_ls.text));}
  if(_ls.services.size>0){list=list.filter(l=>_ls.services.has(l.service_name));}
  if(_ls.levels.size>0){
    list=list.filter(l=>{
      const n=l.severity_number||0;
      return[..._ls.levels].some(lbl=>{const[lo,hi]=sevRange(lbl);return n>=lo&&n<=hi;});
    });
  }
  const total=list.length;
  const countEl=document.getElementById('logs-count');
  if(countEl)countEl.textContent=total;
  const tbody=document.getElementById('logs-tbody');
  if(!tbody)return;
  if(!total){
    tbody.innerHTML='<tr class="empty"><td colspan="5">No logs found</td></tr>';
    renderPager('logs-pager',1,0,_ls.pageSize,'setLogsPage');
    return;
  }
  const start=(_ls.page-1)*_ls.pageSize;
  const page=list.slice(start,start+_ls.pageSize);
  tbody.innerHTML=page.map(l=>{
    const ts=l.log_timestamp>0?fmtNano(l.log_timestamp):fmtTs(l.timestamp);
    const sn=l.severity_number||0;
    const trLink=l.trace_id
      ?` + "`" + `<span class="trace-id" style="cursor:pointer" onclick="event.stopPropagation();go('/trace/${l.trace_id}')">${shortId(l.trace_id)}</span>` + "`" + `
      :'<span class="muted">—</span>';
    return` + "`" + `<tr onclick="toggleLog(this)" style="cursor:pointer">
      <td class="muted small">${ts}</td>
      <td><span class="sev ${sevClass(sn)}">${esc(sevLabel(sn,l.severity_text))}</span></td>
      <td style="color:${svcColor(l.service_name||'unknown')}">${esc(l.service_name||'—')}</td>
      <td>${trLink}</td>
      <td class="log-msg">${esc(l.body||'')}</td>
    </tr>` + "`" + `;
  }).join('');
  renderPager('logs-pager',_ls.page,total,_ls.pageSize,'setLogsPage');
}

// ── Page: Trace Detail ─────────────────────────────────────────────────────

async function renderTraceDetail(traceId){
  const app=document.getElementById('app');
  app.innerHTML='<div class="loading">Loading trace…</div>';
  let data;
  try{data=await apiFetch('/api/traces/'+encodeURIComponent(traceId))}
  catch(e){app.innerHTML=err('Failed to load trace: '+e.message);return}

  const spans=data.spans||[];
  if(!spans.length){
    app.innerHTML=` + "`" + `<div class="page"><button class="back-btn" onclick="go('/traces')">← Traces</button><div class="empty-state">No spans found for trace ${esc(traceId)}</div></div>` + "`" + `;
    return;
  }

  // Build tree
  const byId={};spans.forEach(s=>byId[s.span_id]=s);
  const roots=[];const kids={};
  spans.forEach(s=>{
    if(!s.parent_span_id||!byId[s.parent_span_id])roots.push(s);
    else(kids[s.parent_span_id]=kids[s.parent_span_id]||[]).push(s);
  });
  const ordered=[];
  function dfs(s,d){
    ordered.push({s,d});
    (kids[s.span_id]||[]).sort((a,b)=>a.start_time-b.start_time).forEach(c=>dfs(c,d+1));
  }
  roots.sort((a,b)=>a.start_time-b.start_time).forEach(r=>dfs(r,0));

  // Timeline extents
  const validStarts=spans.map(s=>s.start_time).filter(Boolean);
  const validEnds=spans.map(s=>s.end_time).filter(Boolean);
  const t0=validStarts.length?Math.min(...validStarts):0;
  const t1=validEnds.length?Math.max(...validEnds):0;
  const total=(t1-t0)||1;

  // Root span & attrs
  const root=roots[0]||spans[0];
  const services=[...new Set(spans.map(s=>s.service_name).filter(Boolean))];

  let attrs=[];
  if(root.attributes){
    try{
      const p=typeof root.attributes==='string'?JSON.parse(root.attributes):root.attributes;
      if(Array.isArray(p))attrs=p.map(a=>({key:a.key,val:String(a.value?Object.values(a.value)[0]:'')}));
    }catch(_){}
  }

  // Time axis ticks
  const ticks=[0,.25,.5,.75,1].map(pct=>({pct:pct*100,lbl:fmtDur(pct*total)}));

  app.innerHTML=` + "`" + `
  <div class="page">
    <button class="back-btn" onclick="go('/traces')">← Traces</button>

    <div class="detail-header">
      <div class="detail-trace-id">${esc(traceId)}</div>
      <div class="detail-meta">
        <div class="detail-meta-item">
          <span class="detail-meta-label">Duration</span>
          <span class="detail-meta-value">${fmtDur(total)}</span>
        </div>
        <div class="detail-meta-item">
          <span class="detail-meta-label">Spans</span>
          <span class="detail-meta-value">${spans.length}</span>
        </div>
        <div class="detail-meta-item">
          <span class="detail-meta-label">Start</span>
          <span class="detail-meta-value small">${fmtNano(t0)}</span>
        </div>
        <div class="detail-meta-item">
          <span class="detail-meta-label">Services</span>
          <span class="detail-meta-value">${services.map(s=>'<span style="color:'+svcColor(s)+'">'+esc(s)+'</span>').join(' ')}</span>
        </div>
        <div class="detail-meta-item">
          <span class="detail-meta-label">Status</span>
          <span class="detail-meta-value" style="${statusCls(root.status_code)}">${statusLbl(root.status_code)}</span>
        </div>
      </div>
    </div>

    <a class="logs-link" onclick="go('/logs/${traceId}')">📋 View logs for this trace →</a>

    ${attrs.length?` + "`" + `
    <div class="section">
      <div class="section-header">Root Span Attributes <span class="badge">${attrs.length}</span></div>
      <div class="section-body">
        <table class="attr-table">
          ${attrs.map(a=>'<tr><td class="attr-key">'+esc(a.key)+'</td><td class="attr-val">'+esc(a.val)+'</td></tr>').join('')}
        </table>
      </div>
    </div>` + "`" + `:''}

    <div class="section">
      <div class="section-header">Waterfall <span class="badge">${spans.length} spans</span></div>
      <div class="section-body waterfall-wrap">
        <table class="wf-table">
          <thead><tr>
            <th class="wf-col-name">Span</th>
            <th class="wf-col-svc">Service</th>
            <th class="wf-col-dur">Duration</th>
            <th class="th-timeline">
              <div class="time-axis">
                ${ticks.map(t=>'<div class="t-tick" style="left:'+t.pct+'%">'+t.lbl+'</div>').join('')}
              </div>
            </th>
          </tr></thead>
          <tbody>
          ${ordered.map(({s,d})=>{
            const dur=s.end_time&&s.start_time?s.end_time-s.start_time:0;
            const left=s.start_time?Math.max(0,(s.start_time-t0)/total*100):0;
            const width=Math.max(0.3,dur/total*100);
            const clr=svcColor(s.service_name||'');
            return` + "`" + `<tr class="wf-span-row" onclick="toggleSpanDetail(this,'${s.span_id}')">
              <td class="wf-col-name">
                <div class="span-name-wrap" style="padding-left:${d*16}px">
                  <div class="span-dot" style="background:${clr}"></div>
                  <span class="span-name-text" title="${esc(s.span_name)}">${esc(s.span_name||'—')}</span>
                </div>
              </td>
              <td class="wf-col-svc" style="color:${clr}">${spanSvcLabel(s.service_name,s.activity_source)}</td>
              <td class="wf-col-dur">${fmtDur(dur)}</td>
              <td class="wf-col-bar">
                <div class="bar-track">
                  <div class="bar" style="left:${left.toFixed(2)}%;width:${width.toFixed(2)}%;background:${clr}" title="${esc(s.span_name)} — ${fmtDur(dur)}"></div>
                </div>
              </td>
            </tr>` + "`" + `;
          }).join('')}
          </tbody>
        </table>
      </div>
    </div>
  </div>` + "`" + `;

  // Store span data for onclick detail expansion
  window._wfSpans={};
  ordered.forEach(({s})=>{window._wfSpans[s.span_id]=s;});
}

function toggleLog(row){
  const td=row.querySelector('.log-msg');
  if(td)td.classList.toggle('expanded');
}

function toggleSpanDetail(row,spanId){
  const next=row.nextElementSibling;
  if(next&&next.classList.contains('span-detail-row')){
    next.remove();row.classList.remove('span-row-open');return;
  }
  document.querySelectorAll('.span-detail-row').forEach(r=>r.remove());
  document.querySelectorAll('.span-row-open').forEach(r=>r.classList.remove('span-row-open'));

  const s=window._wfSpans&&window._wfSpans[spanId];
  if(!s)return;

  let attrs=[];
  if(s.attributes){
    try{
      const p=typeof s.attributes==='string'?JSON.parse(s.attributes):s.attributes;
      if(Array.isArray(p))attrs=p.map(a=>({key:a.key,val:String(a.value?Object.values(a.value)[0]:'')}));
    }catch(_){}
  }

  const detail=document.createElement('tr');
  detail.className='span-detail-row';
  const inner=attrs.length
    ?'<table class="attr-table">'+attrs.map(a=>'<tr><td class="attr-key">'+esc(a.key)+'</td><td class="attr-val">'+esc(a.val)+'</td></tr>').join('')+'</table>'
    :'<span class="no-attrs">No attributes</span>';
  detail.innerHTML='<td colspan="4" class="span-detail-cell">'+inner+'</td>';
  row.insertAdjacentElement('afterend',detail);
  row.classList.add('span-row-open');
}

function err(msg){return'<div class="page"><div class="empty-state">'+esc(msg)+'</div></div>'}
</script>
</body>
</html>`
