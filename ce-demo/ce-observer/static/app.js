// CE-Observer — dedicated dashboard for the CE-Manager demo.
// Lanes are CE-only; no finance, SPARC, IBAC lanes.

const stageConfig = [
  { key: "user", label: "User" },
  { key: "ce-demo-agent", label: "CE Demo Agent" },
  { key: "ce-manager", label: "CE Manager" },
  { key: "watsonx", label: "Watsonx" },
  { key: "network", label: "Network" },
];

const CONTEXT_MAX_TOKENS = 131072;

let selectedSessionId = null;
let eventSource = null;
let currentSession = null;

const sessionListEl   = document.getElementById("sessionList");
const sessionTitleEl  = document.getElementById("sessionTitle");
const sessionMetaEl   = document.getElementById("sessionMeta");
const summaryBannerEl = document.getElementById("summaryBanner");
const timelineColumnsEl = document.getElementById("timelineColumns");
const liveBadgeEl     = document.getElementById("liveBadge");
const conversationFeedEl = document.getElementById("conversationFeed");
const contextMeterFill   = document.getElementById("contextMeterFill");
const contextTokensEl    = document.getElementById("contextTokens");
const contextPercentEl   = document.getElementById("contextPercent");
const contextMaskCountEl = document.getElementById("contextMaskCount");
const overflowBannerEl   = document.getElementById("overflowBanner");
const overflowTokensEl   = document.getElementById("overflowTokens");
const sparklineEl        = document.getElementById("sparkline");
const costSparklineEl    = document.getElementById("costSparkline");
const latencySparklineEl = document.getElementById("latencySparkline");
const costTotalEl        = document.getElementById("costTotal");
const latencyTotalEl     = document.getElementById("latencyTotal");
const answerPiecesSectionEl = document.getElementById("answerPiecesSection");
const answerPiecesListEl    = document.getElementById("answerPiecesList");
const modeBadgeEl           = document.getElementById("modeBadge");
const contextModalEl        = document.getElementById("contextModal");
const contextModalBodyEl    = document.getElementById("ceModalBody");
const contextModalSubtitleEl = document.getElementById("ceModalSubtitle");
const contextModalTitleEl   = document.getElementById("ceModalTitle");
const contextModalCopyBtn   = document.getElementById("ceModalCopy");
const diffModalEl           = document.getElementById("diffModal");
const diffModalBodyEl       = document.getElementById("ceDiffModalBody");
const diffModalTitleEl      = document.getElementById("ceDiffModalTitle");
const diffModalSubtitleEl   = document.getElementById("ceDiffModalSubtitle");

document.getElementById("refreshSessions").addEventListener("click", loadSessions);

// Modal close wiring: click backdrop / close button / Escape.
contextModalEl.addEventListener("click", (e) => {
  if (e.target && e.target.dataset && e.target.dataset.closeModal === "1") closeContextModal();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !contextModalEl.classList.contains("hidden")) closeContextModal();
});
contextModalCopyBtn.addEventListener("click", () => {
  const text = contextModalCopyBtn.dataset.payload || "";
  if (!text) return;
  navigator.clipboard.writeText(text).then(
    () => { contextModalCopyBtn.textContent = "Copied!"; setTimeout(() => contextModalCopyBtn.textContent = "Copy", 1200); },
    () => { contextModalCopyBtn.textContent = "Copy failed"; setTimeout(() => contextModalCopyBtn.textContent = "Copy", 1200); },
  );
});

// Diff modal wiring
diffModalEl.addEventListener("click", (e) => {
  if (e.target && e.target.dataset && e.target.dataset.closeDiffModal === "1") closeDiffModal();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !diffModalEl.classList.contains("hidden")) closeDiffModal();
});

// ---------- formatting helpers ----------

function formatTime(v) {
  if (!v) return "";
  return new Date(v).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function formatInt(n) {
  return (typeof n === "number" ? n : Number(n) || 0).toLocaleString();
}

function formatCost(v) {
  if (typeof v !== "number" || !isFinite(v)) return "—";
  if (v === 0) return "$0";
  if (v < 0.001) return `$${v.toFixed(6)}`;
  if (v < 0.01) return `$${v.toFixed(5)}`;
  return `$${v.toFixed(4)}`;
}

function formatLatency(ms) {
  if (typeof ms !== "number" || !isFinite(ms)) return "—";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

// Label helper for compaction method.
function methodLabel(m) {
  switch (m) {
    case "programmatic": return "REWRITE";
    case "summary":      return "SUMMARIZER";
    case "truncate":     return "TRUNCATION";
    case "off":          return "NO CE";
    default:             return "CE";
  }
}

// Label helper for an event's human-friendly action name given the method.
function maskActionLabel(method) {
  switch (method) {
    case "programmatic": return "rewritten";
    case "summary":      return "summarized";
    case "truncate":     return "truncated";
    default:             return "compacted";
  }
}

function shortenMiddle(value, maxLength = 46) {
  if (!value || value.length <= maxLength) return value || "";
  const head = Math.ceil((maxLength - 3) / 2);
  const tail = Math.floor((maxLength - 3) / 2);
  return `${value.slice(0, head)}...${value.slice(value.length - tail)}`;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

// ---------- JSON tree renderer (used by modal + anywhere JSON is shown) ----------

function tryParseJSON(s) {
  if (typeof s !== "string") return null;
  const t = s.trim();
  if (!t) return null;
  if (t[0] !== "{" && t[0] !== "[") return null;
  try { return JSON.parse(t); } catch (_) { return null; }
}

function renderJsonTree(value, indent = 0) {
  // Returns a DocumentFragment with a collapsible JSON tree. Strings that
  // themselves parse as JSON are rendered as nested trees.
  const frag = document.createDocumentFragment();
  const pad = "  ".repeat(indent);

  if (value === null) {
    const s = document.createElement("span");
    s.className = "json-null"; s.textContent = "null";
    frag.appendChild(s); return frag;
  }
  if (typeof value === "boolean") {
    const s = document.createElement("span");
    s.className = "json-boolean"; s.textContent = String(value);
    frag.appendChild(s); return frag;
  }
  if (typeof value === "number") {
    const s = document.createElement("span");
    s.className = "json-number"; s.textContent = String(value);
    frag.appendChild(s); return frag;
  }
  if (typeof value === "string") {
    // Try to decode embedded JSON so nested tool-output strings render as trees.
    const nested = tryParseJSON(value);
    if (nested !== null && (typeof nested === "object")) {
      return renderJsonTree(nested, indent);
    }
    const s = document.createElement("span");
    s.className = "json-string";
    s.textContent = JSON.stringify(value);
    frag.appendChild(s); return frag;
  }
  if (Array.isArray(value)) {
    if (!value.length) {
      const s = document.createElement("span");
      s.className = "json-punc"; s.textContent = "[]";
      frag.appendChild(s); return frag;
    }
    const header = document.createElement("span");
    const toggle = document.createElement("span");
    toggle.className = "json-toggle"; toggle.textContent = "▼";
    const open = document.createElement("span");
    open.className = "json-punc"; open.textContent = "[";
    header.appendChild(toggle); header.appendChild(open);
    const ell = document.createElement("span");
    ell.className = "json-ellipsis"; ell.textContent = `… ${value.length} item${value.length === 1 ? "" : "s"}`;
    ell.style.display = "none";
    header.appendChild(ell);
    frag.appendChild(header);

    const children = document.createElement("div");
    children.className = "json-children";
    value.forEach((item, i) => {
      const row = document.createElement("div");
      row.textContent = pad + "  ";
      row.appendChild(renderJsonTree(item, indent + 1));
      if (i < value.length - 1) {
        const comma = document.createElement("span");
        comma.className = "json-punc"; comma.textContent = ",";
        row.appendChild(comma);
      }
      children.appendChild(row);
    });
    frag.appendChild(children);

    const close = document.createElement("div");
    const cp = document.createElement("span");
    cp.className = "json-punc"; cp.textContent = pad + "]";
    close.appendChild(cp);
    frag.appendChild(close);

    toggle.addEventListener("click", () => {
      const collapsed = children.classList.toggle("collapsed");
      toggle.textContent = collapsed ? "▶" : "▼";
      ell.style.display = collapsed ? "inline" : "none";
      close.style.display = collapsed ? "none" : "block";
    });
    return frag;
  }
  if (typeof value === "object") {
    const keys = Object.keys(value);
    if (!keys.length) {
      const s = document.createElement("span");
      s.className = "json-punc"; s.textContent = "{}";
      frag.appendChild(s); return frag;
    }
    const header = document.createElement("span");
    const toggle = document.createElement("span");
    toggle.className = "json-toggle"; toggle.textContent = "▼";
    const open = document.createElement("span");
    open.className = "json-punc"; open.textContent = "{";
    header.appendChild(toggle); header.appendChild(open);
    const ell = document.createElement("span");
    ell.className = "json-ellipsis";
    ell.textContent = `… ${keys.length} key${keys.length === 1 ? "" : "s"}`;
    ell.style.display = "none";
    header.appendChild(ell);
    frag.appendChild(header);

    const children = document.createElement("div");
    children.className = "json-children";
    keys.forEach((k, i) => {
      const row = document.createElement("div");
      row.textContent = pad + "  ";
      const keyEl = document.createElement("span");
      keyEl.className = "json-key"; keyEl.textContent = JSON.stringify(k);
      row.appendChild(keyEl);
      const colon = document.createElement("span");
      colon.className = "json-punc"; colon.textContent = ": ";
      row.appendChild(colon);
      row.appendChild(renderJsonTree(value[k], indent + 1));
      if (i < keys.length - 1) {
        const comma = document.createElement("span");
        comma.className = "json-punc"; comma.textContent = ",";
        row.appendChild(comma);
      }
      children.appendChild(row);
    });
    frag.appendChild(children);

    const close = document.createElement("div");
    const cp = document.createElement("span");
    cp.className = "json-punc"; cp.textContent = pad + "}";
    close.appendChild(cp);
    frag.appendChild(close);

    toggle.addEventListener("click", () => {
      const collapsed = children.classList.toggle("collapsed");
      toggle.textContent = collapsed ? "▶" : "▼";
      ell.style.display = collapsed ? "inline" : "none";
      close.style.display = collapsed ? "none" : "block";
    });
    return frag;
  }
  // unknown
  const s = document.createElement("span");
  s.textContent = String(value);
  frag.appendChild(s);
  return frag;
}

// ---------- modal ----------

function openContextModal(title, subtitle, messages, rawJson) {
  contextModalTitleEl.textContent = title || "Compacted context";
  contextModalSubtitleEl.textContent = subtitle || "";
  contextModalBodyEl.innerHTML = "";

  const payload = rawJson || JSON.stringify(messages, null, 2);
  contextModalCopyBtn.dataset.payload = payload;

  if (Array.isArray(messages) && messages.length) {
    // Render each message as its own card so the reader can see the shape
    // (system, user, assistant w/ tool_calls, tool). Bodies are themselves
    // JSON-aware so nested tool outputs get a collapsible tree.
    for (let i = 0; i < messages.length; i++) {
      const m = messages[i] || {};
      const wrap = document.createElement("div");
      wrap.className = `json-msg json-role-${escapeHtml(String(m.role || "unknown"))}`;

      const head = document.createElement("div");
      head.className = "json-msg-head";
      const role = document.createElement("span");
      role.className = "role"; role.textContent = m.role || "—";
      head.appendChild(role);
      const idx = document.createElement("span");
      idx.className = "meta"; idx.textContent = `msg ${i}`;
      head.appendChild(idx);
      if (m.tool_call_id) {
        const tc = document.createElement("span");
        tc.className = "meta"; tc.textContent = `tool_call_id=${m.tool_call_id}`;
        head.appendChild(tc);
      }
      const contentChars = typeof m.content === "string" ? m.content.length : 0;
      const sz = document.createElement("span");
      sz.className = "meta";
      sz.textContent = `${formatInt(contentChars)} chars`;
      head.appendChild(sz);
      wrap.appendChild(head);

      const body = document.createElement("div");
      body.className = "json-msg-body";
      // If content is JSON, render tree; else pre-wrap text. Also show tool_calls
      // as JSON if present.
      const content = m.content;
      const nested = typeof content === "string" ? tryParseJSON(content) : null;
      if (nested !== null && typeof nested === "object") {
        body.classList.add("nested");
        const tree = document.createElement("div");
        tree.className = "json-tree";
        tree.appendChild(renderJsonTree(nested));
        body.appendChild(tree);
      } else if (typeof content === "string") {
        body.textContent = content;
      } else if (content != null) {
        body.classList.add("nested");
        const tree = document.createElement("div");
        tree.className = "json-tree";
        tree.appendChild(renderJsonTree(content));
        body.appendChild(tree);
      } else {
        body.textContent = "(no content)";
      }
      wrap.appendChild(body);

      if (m.tool_calls && Array.isArray(m.tool_calls) && m.tool_calls.length) {
        const tcHead = document.createElement("div");
        tcHead.className = "json-msg-head"; tcHead.style.marginTop = "6px";
        const tag = document.createElement("span");
        tag.className = "role"; tag.textContent = "tool_calls";
        tcHead.appendChild(tag);
        wrap.appendChild(tcHead);
        const tcBody = document.createElement("div");
        tcBody.className = "json-msg-body nested";
        const tree = document.createElement("div");
        tree.className = "json-tree";
        tree.appendChild(renderJsonTree(m.tool_calls));
        tcBody.appendChild(tree);
        wrap.appendChild(tcBody);
      }

      contextModalBodyEl.appendChild(wrap);
    }
  } else {
    // Generic: render rawJson / any value as a tree.
    const tree = document.createElement("div");
    tree.className = "json-tree";
    let v = messages;
    if (v == null && rawJson) { try { v = JSON.parse(rawJson); } catch (_) { v = rawJson; } }
    tree.appendChild(renderJsonTree(v == null ? {} : v));
    contextModalBodyEl.appendChild(tree);
  }
  contextModalEl.classList.remove("hidden");
  contextModalEl.setAttribute("aria-hidden", "false");
}

function closeContextModal() {
  contextModalEl.classList.add("hidden");
  contextModalEl.setAttribute("aria-hidden", "true");
}

function closeDiffModal() {
  diffModalEl.classList.add("hidden");
  diffModalEl.setAttribute("aria-hidden", "true");
}

// VS-Code-style unified diff viewer. Takes the per_message array from a
// context_diff event and renders one collapsible hunk per changed message.
function openDiffModal(title, subtitle, perMessage, overallBefore, overallAfter) {
  diffModalTitleEl.textContent = title || "Compaction diff";
  diffModalSubtitleEl.textContent = subtitle || "";
  diffModalBodyEl.innerHTML = "";

  const header = document.createElement("div");
  header.style.padding = "10px 2px";
  header.style.fontSize = "0.82rem";
  header.style.color = "#5d6a75";
  header.textContent =
    `${perMessage.length} message(s) changed · ` +
    `chars ${formatInt(overallBefore || 0)} → ${formatInt(overallAfter || 0)} ` +
    `(−${overallBefore > 0 ? Math.round((1 - (overallAfter || 0) / overallBefore) * 100) : 0}%)`;
  diffModalBodyEl.appendChild(header);

  if (!perMessage || !perMessage.length) {
    const e = document.createElement("div");
    e.className = "vsdiff-empty";
    e.textContent = "No message content changed.";
    diffModalBodyEl.appendChild(e);
  } else {
    for (const entry of perMessage) {
      diffModalBodyEl.appendChild(renderVSDiff(entry));
    }
  }

  diffModalEl.classList.remove("hidden");
  diffModalEl.setAttribute("aria-hidden", "false");
}

function renderVSDiff(entry) {
  const wrap = document.createElement("div");
  wrap.className = "vsdiff";

  const head = document.createElement("div");
  head.className = "vsdiff-head";
  const reduction = entry.chars_before ? Math.round((1 - (entry.chars_after || 0) / entry.chars_before) * 100) : 0;
  const title = document.createElement("span");
  title.textContent =
    `msg #${entry.index} · role=${entry.role}` +
    (entry.tool_call_id ? ` · tc=${entry.tool_call_id}` : "") +
    ` · ${formatInt(entry.chars_before || 0)} → ${formatInt(entry.chars_after || 0)} chars (−${reduction}%)`;
  head.appendChild(title);
  const toggle = document.createElement("span");
  toggle.className = "toggle";
  toggle.textContent = "▼";
  head.appendChild(toggle);
  wrap.appendChild(head);

  const rowsWrap = document.createElement("div");
  rowsWrap.className = "vsdiff-rows";

  if (typeof entry.content_before === "string" && typeof entry.content_after === "string") {
    const lines = lineDiff(entry.content_before, entry.content_after);
    let lbBefore = 0, lbAfter = 0;
    for (const line of lines) {
      const row = document.createElement("div");
      row.className = `vsdiff-row ${line.type}`;

      const leftNo = document.createElement("span");
      leftNo.className = "lineno";
      const rightNo = document.createElement("span");
      rightNo.className = "lineno";

      if (line.type === "same")        { lbBefore++; lbAfter++; leftNo.textContent = lbBefore; rightNo.textContent = lbAfter; }
      else if (line.type === "before") { lbBefore++; leftNo.textContent = lbBefore; rightNo.textContent = ""; row.classList.add("del"); row.classList.remove("before"); }
      else                             { lbAfter++;  leftNo.textContent = "";       rightNo.textContent = lbAfter;  row.classList.add("add"); row.classList.remove("after"); }

      row.appendChild(leftNo);
      row.appendChild(rightNo);

      const content = document.createElement("span");
      content.className = "content";
      content.textContent = line.text;
      row.appendChild(content);

      rowsWrap.appendChild(row);
    }
  } else {
    const e = document.createElement("div");
    e.className = "vsdiff-empty";
    e.textContent = "Content not included in event payload (set CE_EMIT_DIFF_CONTENT=1).";
    rowsWrap.appendChild(e);
  }

  wrap.appendChild(rowsWrap);
  head.addEventListener("click", () => {
    const collapsed = rowsWrap.classList.toggle("collapsed");
    toggle.textContent = collapsed ? "▶" : "▼";
  });
  return wrap;
}

// ---------- lane routing ----------

function eventLane(evt) {
  if (evt.stage === "user_turn") return "user";
  if (evt.source === "watsonx") return "watsonx";
  // CE Manager events that are just forwarding to watsonx → watsonx lane for clarity
  if (evt.source === "ce-manager" && evt.stage === "watsonx_call") return "watsonx";
  if (stageConfig.some((s) => s.key === evt.source)) return evt.source;
  return "network";
}

// ---------- timeline ----------

function createTimelineColumns() {
  timelineColumnsEl.innerHTML = "";
  for (const stage of stageConfig) {
    const col = document.createElement("section");
    col.className = "timeline-column";
    col.dataset.stage = stage.key;

    const title = document.createElement("h3");
    title.textContent = stage.label;
    col.appendChild(title);

    const stack = document.createElement("div");
    stack.className = "event-stack";
    col.appendChild(stack);

    timelineColumnsEl.appendChild(col);
  }
}

function conciseSummary(evt) {
  const data = evt.data || {};

  if (evt.source === "ce-manager") {
    if (evt.stage === "session_cache_hit") {
      const p = ["cache reuse"];
      if (typeof data.cached_prefix_messages === "number") p.push(`prefix=${data.cached_prefix_messages} msgs`);
      if (typeof data.new_turns === "number") p.push(`+${data.new_turns} new`);
      if (typeof data.compact_count === "number") p.push(`compacts=${data.compact_count}`);
      return p.join(" · ");
    }
    if (evt.stage === "session_cache_save") {
      const p = ["cache save"];
      if (typeof data.compacted_messages === "number") p.push(`compacted=${data.compacted_messages} msgs`);
      if (typeof data.raw_len === "number") p.push(`raw_len=${data.raw_len}`);
      if (typeof data.compact_count === "number") p.push(`#${data.compact_count}`);
      return p.join(" · ");
    }
    const parts = [];
    if (typeof data.utilization_pct === "number") parts.push(`${data.utilization_pct}%`);
    if (typeof data.raw_utilization_pct === "number" && data.raw_utilization_pct !== data.utilization_pct) {
      parts.push(`(raw ${data.raw_utilization_pct}%)`);
    }
    if (typeof data.tokens_in === "number" && typeof data.max_tokens === "number") {
      parts.push(`${formatInt(data.tokens_in)}/${formatInt(data.max_tokens)} tok`);
    }
    if (typeof data.tokens_before === "number" && typeof data.tokens_after === "number") {
      parts.push(`${formatInt(data.tokens_before)}→${formatInt(data.tokens_after)} tok`);
    }
    if (typeof data.reduction_pct === "number") parts.push(`−${data.reduction_pct}%`);
    if (typeof data.messages_changed === "number") parts.push(`${data.messages_changed} msgs`);
    if (data.cache_hit) parts.push("cache-hit");
    return parts.join(" · ");
  }

  if (evt.stage === "model_proposal") {
    return data.tool_name ? `tool ${data.tool_name}` : "";
  }
  if (evt.stage === "tool_result") {
    const p = [];
    if (data.tool_name) p.push(data.tool_name);
    if (typeof data.result_size === "number") p.push(`${formatInt(data.result_size)} chars`);
    if (data.highlight && data.highlight.value) p.push(`signal: ${shortenMiddle(data.highlight.value, 40)}`);
    return p.join(" · ");
  }

  return "";
}

function sanitizeEventData(evt) {
  const data = evt.data || {};
  if (!Object.keys(data).length) return null;
  const clone = JSON.parse(JSON.stringify(data));
  // Big payloads — show metadata only in the default JSON dump.
  if (evt.source === "ce-manager" && evt.stage === "context_diff" && Array.isArray(clone.per_message)) {
    clone.per_message = `[${clone.per_message.length} messages — expand card to view diff]`;
  }
  if (evt.source === "ce-manager" && evt.stage === "mask_codegen" && typeof clone.code_preview === "string") {
    clone.code_preview = `[${clone.code_preview.length} chars — expand card to view code]`;
  }
  if (evt.source === "ce-demo-agent" && evt.stage === "tool_result" && typeof clone.tool_output === "string") {
    clone.tool_output = `[${clone.tool_output.length} chars — expand card to view output]`;
  }
  if (evt.source === "ce-manager" && Array.isArray(clone.compacted_messages_full)) {
    clone.compacted_messages_full = `[${clone.compacted_messages_full.length} messages — click "View compacted context"]`;
  }
  if (evt.source === "ce-manager" && evt.stage === "summary_generated" && typeof clone.summary_text === "string") {
    clone.summary_text = `[${clone.summary_text.length} chars — expand card to view summary]`;
  }
  if (evt.stage === "proxy_request_snapshot" && Array.isArray(clone.outbound_messages)) {
    clone.outbound_messages = `[${clone.outbound_messages.length} messages — click "Open full conversation"]`;
  }
  return clone;
}

function lineDiff(before, after) {
  const a = (before || "").split(/\r?\n/);
  const b = (after || "").split(/\r?\n/);
  const n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Int32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out = []; let i = 0, j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) { out.push({ type: "same", text: a[i] }); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ type: "before", text: a[i] }); i++; }
    else { out.push({ type: "after", text: b[j] }); j++; }
  }
  while (i < n) out.push({ type: "before", text: a[i++] });
  while (j < m) out.push({ type: "after", text: b[j++] });
  return out;
}

function renderDiffBlock(before, after) {
  const container = document.createElement("div");
  container.className = "ce-diff";
  for (const line of lineDiff(before, after)) {
    const row = document.createElement("div");
    row.className = `ce-diff-line ce-diff-${line.type}`;
    const marker = line.type === "before" ? "-" : line.type === "after" ? "+" : " ";
    row.textContent = `${marker} ${line.text}`;
    container.appendChild(row);
  }
  return container;
}

function renderContextDiffCard(evt, card) {
  const data = evt.data || {};
  const wrap = document.createElement("div");
  wrap.className = "ce-diff-wrap";
  const perMsg = Array.isArray(data.per_message) ? data.per_message : [];
  if (!perMsg.length) {
    const e = document.createElement("div");
    e.className = "ce-diff-empty";
    e.textContent = "No message content changed.";
    wrap.appendChild(e);
    card.appendChild(wrap);
    return;
  }
  const hdr = document.createElement("div");
  hdr.className = "ce-diff-header";
  const before = data.overall_chars_before ?? 0;
  const after = data.overall_chars_after ?? 0;
  const pct = before > 0 ? Math.round((1 - after / before) * 100) : 0;
  hdr.textContent = `${perMsg.length} message(s) changed · chars ${formatInt(before)} → ${formatInt(after)} (−${pct}%)`;
  wrap.appendChild(hdr);

  for (const entry of perMsg) {
    const d = document.createElement("details");
    d.className = "ce-diff-entry";
    const s = document.createElement("summary");
    const reduction = entry.chars_before ? Math.round((1 - entry.chars_after / entry.chars_before) * 100) : 0;
    s.textContent =
      `msg #${entry.index} · role=${entry.role}${entry.tool_call_id ? ` · tc=${entry.tool_call_id}` : ""}` +
      ` · ${formatInt(entry.chars_before)} → ${formatInt(entry.chars_after)} chars (−${reduction}%)`;
    d.appendChild(s);
    if (typeof entry.content_before === "string" && typeof entry.content_after === "string") {
      d.appendChild(renderDiffBlock(entry.content_before, entry.content_after));
    } else {
      const e = document.createElement("div");
      e.className = "ce-diff-empty";
      e.textContent = "Content not included in event payload.";
      d.appendChild(e);
    }
    wrap.appendChild(d);
  }
  card.appendChild(wrap);
}

function renderCodegenCard(evt, card) {
  const data = evt.data || {};
  if (typeof data.code_preview !== "string" || !data.code_preview.length) return;
  const d = document.createElement("details");
  d.className = "ce-codegen";
  const s = document.createElement("summary");
  const full = data.code_full_len || data.code_preview.length;
  s.textContent = `LLM-generated distill() — preview ${data.code_preview.length}/${full} chars`;
  d.appendChild(s);
  const pre = document.createElement("pre");
  pre.className = "ce-codegen-code";
  pre.textContent = data.code_preview;
  d.appendChild(pre);
  card.appendChild(d);
}

function renderToolOutputCard(evt, card) {
  const data = evt.data || {};
  const output = typeof data.tool_output === "string" ? data.tool_output : null;
  if (!output) return;

  const details = document.createElement("details");
  details.className = "ce-tool-output";
  const summary = document.createElement("summary");
  summary.textContent = `Tool output · ${data.tool_name || "?"} · ${formatInt(output.length)} chars (click to expand)`;
  details.appendChild(summary);

  if (data.highlight && (data.highlight.value || data.highlight.path)) {
    const hl = document.createElement("div");
    hl.className = "ce-tool-highlight";
    hl.innerHTML = `<b>Signal to extract:</b> <code>${escapeHtml(data.highlight.value || "")}</code><br>` +
                   `<b>Path:</b> <code>${escapeHtml(data.highlight.path || "")}</code><br>` +
                   `<b>Why it matters:</b> ${escapeHtml(data.highlight.why || "")}`;
    details.appendChild(hl);
  }

  const pre = document.createElement("pre");
  pre.className = "ce-tool-json";
  // Pretty-print if it looks like JSON; otherwise show raw.
  let pretty = output;
  try { pretty = JSON.stringify(JSON.parse(output), null, 2); } catch (_) { /* raw */ }
  // Highlight the signal value inside the formatted output.
  const hlValue = data.highlight && data.highlight.value;
  if (hlValue) {
    const parts = pretty.split(hlValue);
    if (parts.length > 1) {
      pre.innerHTML = parts.map(escapeHtml).join(`<span class="match">${escapeHtml(hlValue)}</span>`);
    } else {
      pre.textContent = pretty;
    }
  } else {
    pre.textContent = pretty;
  }
  details.appendChild(pre);
  card.appendChild(details);
}

function renderBadges(evt, card) {
  const container = card.querySelector(".event-badges");
  if (!container) return;
  const data = evt.data || {};
  const badges = [];

  if (evt.source === "ce-manager" && evt.stage === "token_count") {
    const util = typeof data.utilization_pct === "number" ? data.utilization_pct : null;
    if (util !== null) {
      let cls = "info";
      if (util >= 100) cls = "danger";
      else if (util >= 80) cls = "warning";
      else if (util >= 50) cls = "info";
      else cls = "good";
      badges.push({ cls, text: `${util}% of ${formatInt(CONTEXT_MAX_TOKENS)}` });
      if (util >= 80) {
        badges.push({ cls: "warning", text: "⚠ LLM context near capacity" });
      }
    }
    if (typeof data.raw_tokens === "number" && data.raw_tokens !== data.tokens_in) {
      badges.push({ cls: "info", text: `raw would have been ${formatInt(data.raw_tokens)}` });
    }
  }
  if (evt.stage === "tool_result" && typeof data.result_size === "number") {
    badges.push({ cls: "info", text: `+${formatInt(data.result_size)} chars` });
  }
  if (evt.stage === "proxy_request" && typeof data.tokens_in === "number") {
    badges.push({ cls: "info", text: `${formatInt(data.tokens_in)} eff tokens` });
    if (data.cache_hit) badges.push({ cls: "good", text: "cache-hit" });
    if (typeof data.new_turns === "number") badges.push({ cls: "info", text: `+${data.new_turns} new turns` });
  }
  if (evt.stage === "mask_applied" && evt.status === "success") {
    badges.push({ cls: "good", text: `−${data.reduction_pct || 0}% tokens` });
    const m = data.compaction_method;
    if (m === "summary")      badges.push({ cls: "method-summary",      text: "method: summary" });
    else if (m === "truncate") badges.push({ cls: "method-truncate",     text: "method: truncate" });
    else if (m === "programmatic") badges.push({ cls: "method-programmatic", text: "method: programmatic" });
    if (typeof data.ce_cost_usd === "number" && data.ce_cost_usd > 0)
      badges.push({ cls: "cost", text: formatCost(data.ce_cost_usd) });
    if (typeof data.total_latency_ms === "number" && data.total_latency_ms > 0)
      badges.push({ cls: "latency", text: formatLatency(data.total_latency_ms) });
  }
  if (evt.stage === "summary_generated") {
    badges.push({ cls: "method-summary", text: `summary · ${formatInt(data.summary_chars || 0)} chars` });
    const sm = data.summarize_meta;
    if (sm) {
      if (typeof sm.cost_usd === "number" && sm.cost_usd > 0) badges.push({ cls: "cost", text: formatCost(sm.cost_usd) });
      if (typeof sm.latency_ms === "number") badges.push({ cls: "latency", text: formatLatency(sm.latency_ms) });
    }
  }
  if (evt.stage === "mask_codegen") {
    const gm = data.generate_meta;
    if (gm) {
      if (typeof gm.cost_usd === "number" && gm.cost_usd > 0) badges.push({ cls: "cost", text: formatCost(gm.cost_usd) });
      if (typeof gm.latency_ms === "number") badges.push({ cls: "latency", text: formatLatency(gm.latency_ms) });
    }
  }
  if (evt.stage === "watsonx_call" && evt.status === "success") {
    if (typeof data.cost_usd === "number") badges.push({ cls: "cost", text: formatCost(data.cost_usd) });
    if (typeof data.latency_ms === "number") badges.push({ cls: "latency", text: formatLatency(data.latency_ms) });
    if (typeof data.prompt_tokens === "number")
      badges.push({ cls: "info", text: `${formatInt(data.prompt_tokens)} in / ${formatInt(data.completion_tokens)} out` });
  }
  if (evt.stage === "truncation_applied") {
    badges.push({ cls: "method-truncate", text: `evicted ${data.removed_pairs || 0} pair(s)` });
  }
  if (evt.stage === "context_overflow") {
    badges.push({ cls: "danger", text: `overflow +${formatInt(data.overflow_by || 0)}` });
  }

  for (const b of badges) {
    const el = document.createElement("span");
    el.className = `event-badge ${b.cls}`;
    el.textContent = b.text;
    container.appendChild(el);
  }
}

function appendEventToTimeline(evt) {
  const lane = eventLane(evt);
  const column = timelineColumnsEl.querySelector(`[data-stage="${lane}"] .event-stack`);
  if (!column) return;

  const card = document.getElementById("eventCardTemplate").content.firstElementChild.cloneNode(true);
  card.classList.add(evt.status || "info");
  card.classList.add(`lane-${lane}`);
  card.querySelector(".event-status").textContent = evt.status || "info";
  card.querySelector(".event-time").textContent = formatTime(evt.timestamp);
  card.querySelector(".event-title").textContent = evt.title || evt.stage;
  card.querySelector(".event-summary").textContent = evt.summary || "";

  const preview = conciseSummary(evt);
  if (preview) {
    const pEl = card.querySelector(".event-preview");
    pEl.classList.remove("hidden");
    pEl.textContent = preview;
  }

  renderBadges(evt, card);

  const sanitized = sanitizeEventData(evt);
  const dataEl = card.querySelector(".event-data");
  const toggleEl = card.querySelector(".event-toggle");
  if (sanitized && Object.keys(sanitized).length > 0) {
    dataEl.textContent = JSON.stringify(sanitized, null, 2);
    toggleEl.classList.remove("hidden");
    toggleEl.addEventListener("click", () => {
      const hidden = dataEl.classList.toggle("hidden");
      toggleEl.textContent = hidden ? "View JSON" : "Hide JSON";
    });
  }

  // CE-specific richer renderings.
  if (evt.source === "ce-manager" && evt.stage === "mask_codegen") {
    renderCodegenCard(evt, card);
  } else if (evt.source === "ce-demo-agent" && evt.stage === "tool_result") {
    renderToolOutputCard(evt, card);
  } else if (evt.source === "ce-manager" && evt.stage === "summary_generated") {
    renderSummaryGeneratedCard(evt, card);
  }

  // Action buttons on event cards: compacted-context modal, VS-Code diff modal,
  // "open full conversation at this step" modal. Collected together in one row.
  const data = evt.data || {};
  const actionRow = document.createElement("div");
  actionRow.className = "event-action-row";
  let anyAction = false;

  // View compacted context modal
  if (evt.source === "ce-manager"
      && (evt.stage === "mask_applied" || evt.stage === "session_cache_hit")
      && Array.isArray(data.compacted_messages_full)
      && data.compacted_messages_full.length) {
    const btn = document.createElement("button");
    btn.type = "button";
    const method = data.compaction_method;
    btn.textContent = evt.stage === "session_cache_hit"
      ? `View compacted context (cached · ${data.compacted_messages_full.length} msgs)`
      : `View compacted context (${method || "result"} · ${data.compacted_messages_full.length} msgs)`;
    btn.addEventListener("click", () => {
      const stageLabel = evt.stage === "session_cache_hit" ? "Cached compacted prefix" : "Compacted context";
      const sub = `method=${method || "—"} · ${data.compacted_messages_full.length} messages · ${evt.title || ""}`;
      openContextModal(stageLabel, sub, data.compacted_messages_full, null);
    });
    actionRow.appendChild(btn);
    anyAction = true;
  }

  // View VS-Code diff modal on context_diff cards
  if (evt.source === "ce-manager" && evt.stage === "context_diff"
      && Array.isArray(data.per_message)) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = `View diff in modal (${data.per_message.length} msg${data.per_message.length === 1 ? "" : "s"})`;
    btn.addEventListener("click", () => {
      const sub = `method=${data.compaction_method || "—"} · ${data.messages_changed || data.per_message.length} changed`;
      openDiffModal("Compaction diff", sub, data.per_message, data.overall_chars_before, data.overall_chars_after);
    });
    actionRow.appendChild(btn);

    // Small inline summary so you don't have to open the modal for a glance.
    const inline = document.createElement("div");
    inline.className = "subtle";
    inline.style.marginTop = "6px";
    inline.style.fontSize = "0.78rem";
    const before = data.overall_chars_before ?? 0;
    const after = data.overall_chars_after ?? 0;
    const pct = before > 0 ? Math.round((1 - after / before) * 100) : 0;
    inline.textContent = `${data.per_message.length} message(s) changed · chars ${formatInt(before)} → ${formatInt(after)} (−${pct}%)`;
    card.appendChild(inline);
    anyAction = true;
  }

  // Open full conversation snapshot on any event that has a corresponding
  // proxy_request_snapshot earlier in the stream.
  if (currentSession && currentSession.events) {
    const snap = findLatestSnapshotBefore(currentSession.events, evt.timestamp);
    if (snap && Array.isArray((snap.data || {}).outbound_messages) && snap.data.outbound_messages.length) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "secondary";
      btn.textContent = `Open full conversation at this step (${snap.data.outbound_messages.length} msgs)`;
      btn.addEventListener("click", () => {
        const method = snap.data.compaction_method || "—";
        const sub = `snapshot from ${formatTime(snap.timestamp)} · method=${method} · ${snap.data.outbound_messages.length} messages`;
        openContextModal("Outbound conversation", sub, snap.data.outbound_messages, null);
      });
      actionRow.appendChild(btn);
      anyAction = true;
    }
  }

  if (anyAction) card.appendChild(actionRow);

  column.appendChild(card);
}

// Walk backwards through the session events to find the most recent
// proxy_request_snapshot with timestamp ≤ target.
function findLatestSnapshotBefore(events, targetTs) {
  if (!targetTs) return null;
  const targetMs = new Date(targetTs).getTime();
  let best = null;
  for (const e of events) {
    if (e.stage !== "proxy_request_snapshot") continue;
    const t = new Date(e.timestamp).getTime();
    if (t > targetMs) continue;
    if (!best || new Date(best.timestamp).getTime() < t) best = e;
  }
  return best;
}

function renderSummaryGeneratedCard(evt, card) {
  const data = evt.data || {};
  if (typeof data.summary_text !== "string" || !data.summary_text.length) return;
  const body = document.createElement("div");
  body.className = "ce-summary-body";
  body.textContent = data.summary_text;
  card.appendChild(body);
}

// ---------- conversation feed ----------

function conversationItems(events) {
  const items = [];
  for (const evt of events) {
    // Note: ce-manager/context_overflow is intentionally filtered OUT here —
    // the agent's own agent_blocked card already tells that story, and the
    // red banner at the top of the session view is still visible. Showing
    // both produces a duplicate "context overflow" row that clutters the feed.
    const include =
      evt.stage === "user_turn" ||
      evt.stage === "assistant_reply" ||
      evt.stage === "model_proposal" ||
      (evt.source === "ce-demo-agent" && evt.stage === "tool_result") ||
      (evt.source === "ce-manager" && evt.stage === "mask_applied") ||
      (evt.source === "ce-demo-agent" && evt.stage === "agent_blocked");
    if (!include) continue;
    items.push(evt);
  }
  return items;
}

function conversationEntry(evt) {
  const data = evt.data || {};
  if (evt.stage === "user_turn") {
    return { roleClass: "user", speaker: "User", kind: "User request", text: evt.summary || "" };
  }
  if (evt.stage === "assistant_reply") {
    return {
      roleClass: "agent",
      speaker: "CE Demo Agent",
      kind: "Agent reply",
      text: evt.summary || evt.raw_log || "",
    };
  }
  if (evt.stage === "model_proposal") {
    const toolName = data.tool_name || "tool";
    const args = data.arguments ? JSON.stringify(data.arguments) : "";
    return {
      roleClass: "tool",
      speaker: "CE Demo Agent",
      kind: "Tool call",
      text: `${toolName}${args ? ` ${args}` : ""}`,
    };
  }
  if (evt.source === "ce-demo-agent" && evt.stage === "tool_result") {
    const hl = data.highlight || {};
    const head = `${data.tool_name || "tool"} → ${formatInt(data.result_size || 0)} chars`;
    const signal = hl.value ? `\nsignal: ${hl.value}` : "";
    return {
      roleClass: "tool",
      speaker: "Tool output",
      kind: "Tool output",
      text: head + signal,
    };
  }
  if (evt.source === "ce-manager" && evt.stage === "mask_applied") {
    const method = data.compaction_method || "programmatic";
    const action = maskActionLabel(method);
    return {
      roleClass: "guard",
      speaker: "CE Manager",
      kind: `Context ${action}`,
      text: `Compacted context from ${formatInt(data.tokens_before ?? 0)} to ${formatInt(data.tokens_after ?? 0)} tokens (−${data.reduction_pct ?? 0}%) via ${method}. ${data.messages_changed ?? 0} message(s) changed.`,
    };
  }
  if (evt.source === "ce-demo-agent" && evt.stage === "agent_blocked") {
    return {
      roleClass: "guard",
      speaker: "CE Demo Agent",
      kind: "Blocked",
      text: evt.summary || "Agent blocked by context overflow.",
    };
  }
  return null;
}

// Aggregate turn-level stats: walk event stream, partition by user_turn
// boundaries, sum watsonx + CE-call cost/time for each turn.
function computeTurnRollups(events) {
  const rollups = [];
  let current = null;
  const finalize = () => {
    if (current) rollups.push(current);
  };
  for (const evt of events) {
    if (evt.stage === "user_turn") {
      finalize();
      current = {
        turnNumber: rollups.length + 1,
        startTs: evt.timestamp,
        endTs: evt.timestamp,
        query: (evt.summary || "").toString(),
        watsonxCalls: 0,
        compactions: 0,
        costUsd: 0,
        latencyMs: 0,
        method: "off",
      };
      continue;
    }
    if (!current) continue;
    current.endTs = evt.timestamp || current.endTs;
    const d = evt.data || {};
    if (d.compaction_method && evt.source === "ce-manager") current.method = d.compaction_method;
    if (evt.source === "ce-manager" && evt.stage === "watsonx_call" && evt.status === "success") {
      current.watsonxCalls += 1;
      current.costUsd += Number(d.cost_usd || 0);
      current.latencyMs += Number(d.latency_ms || 0);
    }
    if (evt.source === "ce-manager" && evt.stage === "mask_applied" && evt.status === "success") {
      current.compactions += 1;
      current.costUsd += Number(d.ce_cost_usd || 0);
      current.latencyMs += Number(d.ce_latency_ms || d.total_latency_ms || 0);
    }
    if (evt.stage === "assistant_reply") {
      current.endTs = evt.timestamp;
    }
  }
  finalize();
  return rollups;
}

function renderTurnSummaryCard(roll) {
  const wrap = document.createElement("div");
  wrap.className = "turn-summary-card";
  const badge = document.createElement("span");
  badge.className = "turn-badge";
  badge.textContent = `TURN ${roll.turnNumber}`;
  wrap.appendChild(badge);

  const q = document.createElement("span");
  q.className = "turn-query-text";
  q.title = roll.query || "";
  q.textContent = roll.query || "(no query captured)";
  wrap.appendChild(q);

  const stats = document.createElement("span");
  stats.className = "turn-stats";
  const elapsedMs = (roll.startTs && roll.endTs)
    ? Math.max(0, new Date(roll.endTs).getTime() - new Date(roll.startTs).getTime())
    : 0;
  stats.innerHTML =
    `<span class="stat"><span class="label">method</span>${escapeHtml(methodLabel(roll.method))}</span>` +
    `<span class="stat"><span class="label">watsonx</span>${roll.watsonxCalls}×</span>` +
    `<span class="stat"><span class="label">compact</span>${roll.compactions}×</span>` +
    `<span class="stat"><span class="label">time</span>${formatLatency(elapsedMs)}</span>` +
    `<span class="stat"><span class="label">cost</span>${formatCost(roll.costUsd)}</span>`;
  wrap.appendChild(stats);
  return wrap;
}

function renderConversation(events) {
  const items = conversationItems(events);
  if (!items.length) {
    conversationFeedEl.className = "conversation-feed empty-state";
    conversationFeedEl.textContent = "Waiting for the first user turn — conversation will stream here.";
    return;
  }
  conversationFeedEl.className = "conversation-feed";
  conversationFeedEl.innerHTML = "";

  const rollups = computeTurnRollups(events);
  const rollupsByTs = new Map();
  for (const r of rollups) rollupsByTs.set(r.startTs, r);

  for (const evt of items) {
    // Insert a turn-summary card right before the user_turn conversation item.
    if (evt.stage === "user_turn") {
      const roll = rollupsByTs.get(evt.timestamp);
      if (roll) conversationFeedEl.appendChild(renderTurnSummaryCard(roll));
    }
    const e = conversationEntry(evt);
    if (!e) continue;
    const item = document.getElementById("conversationItemTemplate").content.firstElementChild.cloneNode(true);
    item.classList.add(e.roleClass);
    item.querySelector(".conversation-speaker").textContent = e.speaker;
    item.querySelector(".conversation-time").textContent = formatTime(evt.timestamp);
    item.querySelector(".conversation-kind").textContent = e.kind;
    item.querySelector(".conversation-text").textContent = e.text;

    // Action buttons: "View full conversation" opens the outbound snapshot
    // at this step; "View new compacted context" (mask_applied only) opens
    // the post-compaction message array.
    const viewFullBtn = item.querySelector(".conv-btn-full");
    const viewCompactedBtn = item.querySelector(".conv-btn-compacted");
    const snap = findLatestSnapshotBefore(events, evt.timestamp);
    if (snap && Array.isArray((snap.data || {}).outbound_messages) && snap.data.outbound_messages.length) {
      viewFullBtn.classList.remove("hidden");
      viewFullBtn.addEventListener("click", () => {
        const method = snap.data.compaction_method || "—";
        const sub = `snapshot from ${formatTime(snap.timestamp)} · method=${method} · ${snap.data.outbound_messages.length} messages`;
        openContextModal("Full conversation", sub, snap.data.outbound_messages, null);
      });
    }
    if (evt.source === "ce-manager" && evt.stage === "mask_applied"
        && Array.isArray((evt.data || {}).compacted_messages_full)
        && evt.data.compacted_messages_full.length) {
      viewCompactedBtn.classList.remove("hidden");
      viewCompactedBtn.addEventListener("click", () => {
        const method = (evt.data || {}).compaction_method || "—";
        const n = evt.data.compacted_messages_full.length;
        const sub = `method=${method} · ${n} messages · ${evt.title || ""}`;
        openContextModal("New compacted context", sub, evt.data.compacted_messages_full, null);
      });
    }

    conversationFeedEl.appendChild(item);
  }
}

// ---------- context meter + sparkline ----------

function updateContextMeter(events) {
  let latest = null;
  let maskCount = 0;
  let overflowTokens = null;
  let latestMode = null;
  let latestMethod = null;
  for (const e of events) {
    if (e.source === "ce-manager" && e.stage === "token_count") {
      latest = e;
    }
    if (e.source === "ce-manager" && e.stage === "mask_applied" && e.status === "success") maskCount++;
    if (e.source === "ce-manager" && e.stage === "context_overflow") overflowTokens = (e.data || {}).tokens_in;
    if (e.source === "ce-manager" && e.stage === "proxy_request" && (e.data || {}).mode) latestMode = e.data.mode;
    if (e.source === "ce-manager" && (e.data || {}).compaction_method) latestMethod = e.data.compaction_method;
  }
  // Reflect mode + method in the badge next to the "Context window" header.
  if (latestMode) {
    const knownModes = ["summary", "on", "off", "truncate"];
    const cls = knownModes.includes(latestMode) ? latestMode : "off";
    let txt = `CE_MODE=${latestMode}`;
    if (latestMode !== "off" && latestMethod) txt += ` · ${latestMethod}`;
    modeBadgeEl.className = `mode-badge ${cls}`;
    modeBadgeEl.textContent = txt;
    modeBadgeEl.classList.remove("hidden");
  } else {
    modeBadgeEl.classList.add("hidden");
  }
  if (latest) {
    const tokens = (latest.data || {}).tokens_in ?? 0;
    const pct = Math.min(150, (latest.data || {}).utilization_pct ?? 0);
    contextTokensEl.textContent = `${formatInt(tokens)} / ${formatInt(CONTEXT_MAX_TOKENS)} tokens`;
    contextPercentEl.textContent = `${pct}%`;
    contextMeterFill.style.width = `${Math.min(pct, 100)}%`;
  } else {
    contextTokensEl.textContent = `0 / ${formatInt(CONTEXT_MAX_TOKENS)} tokens`;
    contextPercentEl.textContent = "0%";
    contextMeterFill.style.width = "0%";
  }
  contextMaskCountEl.textContent = `${maskCount} compaction${maskCount === 1 ? "" : "s"}`;

  if (overflowTokens !== null) {
    overflowBannerEl.classList.remove("hidden");
    overflowTokensEl.textContent = formatInt(overflowTokens);
    contextMeterFill.style.width = "100%";
    contextMeterFill.style.background = "#a32735";
  } else {
    overflowBannerEl.classList.add("hidden");
    contextMeterFill.style.background = "";
  }
}

// ---------- answer-pieces tracker ----------
//
// The agent emits a `scenario_intro` event at the start of each turn with the
// list of small facts it needs to collect before it can answer. Subsequent
// `answer_piece_collected` events mark each fact as gathered. After every
// `context_diff` event we rescan the compacted messages to check whether each
// collected piece's value is still present, so the UI can warn if the masker
// removed something load-bearing.

// One turn = one user question. Walk the event stream; every scenario_intro
// opens a new turn, and every answer_piece_collected / context_diff updates
// the currently-open turn's pieces.
// Convert any value into a single string to grep for an answer-piece Example.
function messagesAsText(messages) {
  if (!Array.isArray(messages)) return "";
  const parts = [];
  for (const m of messages) {
    if (!m) continue;
    const c = m.content;
    if (typeof c === "string") parts.push(c);
    else if (c != null) {
      try { parts.push(JSON.stringify(c)); } catch (_) { /* ignore */ }
    }
    if (m.tool_calls) {
      try { parts.push(JSON.stringify(m.tool_calls)); } catch (_) { /* ignore */ }
    }
  }
  return parts.join("\n");
}

function computeTurnGroups(events) {
  const turns = [];
  let current = null;
  let userTurnCount = 0;
  let latestMethod = "off";
  // Chronological list of compaction events that carry compacted_messages_full.
  // Per-piece drop detection walks this list once, from the piece's source
  // event onwards, and records the FIRST compaction whose compacted view
  // doesn't contain the piece value. That's the event that dropped it.
  const compactions = [];
  // Last outbound snapshot (used as a fallback lens when no compaction has
  // fired at all — lets us detect "dropped" cases in off-mode as well).
  let latestSnapshot = null;

  for (const e of events) {
    if (e.stage === "user_turn") userTurnCount += 1;

    if (e.source === "ce-manager"
        && (e.stage === "mask_applied" || e.stage === "summary_applied"
            || e.stage === "truncation_applied" || e.stage === "session_cache_hit")
        && Array.isArray((e.data || {}).compacted_messages_full)) {
      compactions.push(e);
      if ((e.data || {}).compaction_method) latestMethod = e.data.compaction_method;
    }
    if (e.stage === "proxy_request_snapshot" && Array.isArray((e.data || {}).outbound_messages)) {
      latestSnapshot = e;
      if ((e.data || {}).compaction_method) latestMethod = e.data.compaction_method;
    }
    if ((e.data || {}).compaction_method && e.source === "ce-manager") {
      latestMethod = e.data.compaction_method;
    }

    if (e.stage === "scenario_intro") {
      const ap = (e.data || {}).answer_pieces;
      if (!Array.isArray(ap)) continue;
      current = {
        turnNumber: userTurnCount || turns.length + 1,
        query: (e.data || {}).query || "",
        method: latestMethod,
        pieces: ap.map((p) => ({
          ...p,
          collected: false,
          collected_value: null,
          survived: null,
          sourceEvent: null,
          droppedByEvent: null,
        })),
        verdict: null,
        reply: "",
      };
      turns.push(current);
    } else if (e.stage === "answer_piece_collected" && current) {
      const d = e.data || {};
      const hit = current.pieces.find((p) => p.name === d.name);
      // Use the emitted value only — no fallback to `example`, which would
      // manufacture a value the agent never actually saw and can lead to
      // false "dropped" verdicts. If the agent emitted an empty value we
      // treat the piece as not-yet-collected (it's an emit bug, not a drop).
      if (hit && typeof d.value === "string" && d.value.length > 0) {
        hit.collected = true;
        hit.collected_value = d.value;
        hit.sourceEvent = e;
      }
    } else if (e.stage === "assistant_reply" && current) {
      current.reply = (e.summary || (e.data || {}).message || "").toString();
    }
  }

  // Per-piece drop attribution.
  //
  // Rule: a piece is "present" iff the LATEST effective context contains
  // its value. "Latest effective context" = the most recent compaction
  // after the piece's sourceEvent if any, else the most recent outbound
  // snapshot. Intermediate compactions that briefly dropped a value are
  // recorded in `everDropped`/`droppedByEvent` but DO NOT sink the final
  // verdict — if a subsequent compaction or snapshot re-introduces the
  // value (e.g., because the rewrite preserved it verbatim, or the agent
  // re-invoked the tool), the piece is reported as survived.
  for (const t of turns) {
    for (const p of t.pieces) {
      if (!p.collected || !p.collected_value) continue;
      const needle = String(p.collected_value).toLowerCase();
      const srcTs = p.sourceEvent ? new Date(p.sourceEvent.timestamp).getTime() : 0;
      const subsequent = compactions.filter(
        (c) => new Date(c.timestamp).getTime() >= srcTs,
      );

      // Record a "briefly dropped" signal — first subsequent compaction
      // that lost the needle. Informational only; does not commit survival.
      let droppedAt = null;
      for (const c of subsequent) {
        const hay = messagesAsText(c.data.compacted_messages_full).toLowerCase();
        if (!hay.includes(needle)) { droppedAt = c; break; }
      }

      // Compute "present now" against the LATEST effective context.
      let presentNow = null;
      const latestCompaction = subsequent.length ? subsequent[subsequent.length - 1] : null;
      const snapTs = latestSnapshot ? new Date(latestSnapshot.timestamp).getTime() : 0;
      const compTs = latestCompaction ? new Date(latestCompaction.timestamp).getTime() : 0;
      if (latestSnapshot && snapTs >= compTs && snapTs >= srcTs) {
        const hay = messagesAsText(latestSnapshot.data.outbound_messages).toLowerCase();
        presentNow = hay.includes(needle);
      } else if (latestCompaction) {
        const hay = messagesAsText(latestCompaction.data.compacted_messages_full).toLowerCase();
        presentNow = hay.includes(needle);
      } else if (latestSnapshot && snapTs >= srcTs) {
        const hay = messagesAsText(latestSnapshot.data.outbound_messages).toLowerCase();
        presentNow = hay.includes(needle);
      }

      if (presentNow === null) {
        // No context to check against — leave survived as null (unknown).
        p.survived = null;
      } else {
        p.survived = presentNow;
      }
      p.everDropped = Boolean(droppedAt);
      // Only surface droppedByEvent if the piece is NOT in the latest
      // context. If it came back, don't attribute the drop to any event.
      p.droppedByEvent = (p.survived === false) ? droppedAt : null;
    }
  }

  // Apply the correct/incorrect verdict per turn based on the final reply.
  for (const t of turns) {
    t.verdict = verifyAnswer(t, t.reply);
  }
  return turns;
}

// Compute { ok, missing, expected, actual } for a turn.
function verifyAnswer(turn, replyText) {
  const reply = (replyText || "").trim();
  if (!reply) return { ok: null, missing: [], expected: "", actual: "" };

  // Q2-shape has exactly one piece named "others".
  const isQ2 = turn.pieces.length === 1 && turn.pieces[0].name === "others";
  if (isQ2) {
    const expectedRaw = turn.pieces[0].example || "";
    const want = expectedRaw
      .split(",")
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean);
    const normalized = reply.toLowerCase();
    if (normalized.includes("alice.chen@acme.corp")) {
      return { ok: false, missing: ["contains alice (should be excluded)"], expected: expectedRaw, actual: reply };
    }
    const missing = want.filter((w) => !normalized.includes(w));
    return { ok: missing.length === 0, missing, expected: expectedRaw, actual: reply };
  }

  // Q1-shape: extract the ANSWER: line if present, else use full reply.
  let line = reply;
  const m = reply.match(/ANSWER:\s*(.+)/i);
  if (m) line = m[1].trim();

  const missing = [];
  const lineLower = line.toLowerCase();
  const expectedParts = turn.pieces.map((p) => p.example || "");
  for (const p of turn.pieces) {
    const val = String(p.example || "").toLowerCase();
    if (!val) continue;
    if (answerPieceMatches(lineLower, val, p.name)) continue;
    missing.push(p.name.toUpperCase());
  }
  return {
    ok: missing.length === 0,
    missing,
    expected: expectedParts.join(" · "),
    actual: line,
  };
}

// Lenient per-piece match. Exact substring is always fine; for
// browser/version pieces we also accept a trailing-zero-trimmed form
// ("Firefox/120" counts as "Firefox/120.0"). Other pieces still
// require exact substring so decoys never pass.
function answerPieceMatches(lineLower, expectedLower, pieceName) {
  if (lineLower.includes(expectedLower)) return true;
  const name = String(pieceName || "").toLowerCase();
  if (name === "user_agent" || name.includes("browser") || name.includes("user-agent")) {
    // Strip a single trailing ".0" from the version suffix in the
    // expected value (e.g. "firefox/120.0" → "firefox/120") and
    // re-check.
    const trimmed = expectedLower.replace(/(\/\d+)\.0\b/, "$1");
    if (trimmed !== expectedLower && lineLower.includes(trimmed)) return true;
    // Also allow the line to have the trailing ".0" while the expected
    // doesn't (symmetric case).
    const padded = expectedLower.replace(/(\/\d+)\b(?!\.)/, "$1.0");
    if (padded !== expectedLower && lineLower.includes(padded)) return true;
  }
  return false;
}

function renderAnswerPieces(events) {
  const turns = computeTurnGroups(events);
  answerPiecesSectionEl.classList.remove("hidden");
  answerPiecesListEl.innerHTML = "";
  if (!turns.length) {
    const empty = document.createElement("div");
    empty.className = "answer-pieces-empty";
    empty.textContent = "Waiting for the agent to begin tool calls — the answer-pieces checklist appears after the first user turn.";
    answerPiecesListEl.appendChild(empty);
    return;
  }

  turns.forEach((turn, idx) => {
    const group = document.createElement("div");
    group.className = "answer-pieces-turn";

    const methodLbl = methodLabel(turn.method);
    const methodClass = `method-${turn.method || "off"}`;

    // Verdict pill (✓ / ✗ / pending)
    if (turn.verdict && turn.verdict.ok !== null && turn.reply) {
      const v = turn.verdict;
      const pill = document.createElement("div");
      if (v.ok) {
        pill.className = "verdict-pill correct";
        pill.innerHTML = `<span class="mark">✓</span> Correct answer`;
      } else {
        pill.className = "verdict-pill incorrect";
        const miss = v.missing.length ? ` — missing: ${v.missing.join(", ")}` : "";
        pill.innerHTML = `<span class="mark">✗</span> Incorrect answer${escapeHtml(miss)}`;
      }
      group.appendChild(pill);

      const details = document.createElement("details");
      details.className = "verdict-details";
      const summary = document.createElement("summary");
      summary.textContent = "expected vs actual";
      details.appendChild(summary);
      const ea = document.createElement("div");
      ea.className = "expected-actual";
      ea.innerHTML =
        `<b>expected</b><span>${escapeHtml(v.expected || "(unknown)")}</span>` +
        `<b>actual</b><span>${escapeHtml(v.actual || "(no reply)")}</span>`;
      details.appendChild(ea);
      group.appendChild(details);
    } else if (turn.reply === "") {
      const pill = document.createElement("div");
      pill.className = "verdict-pill pending";
      pill.innerHTML = `<span class="mark">…</span> Waiting for agent reply`;
      group.appendChild(pill);
    }

    const head = document.createElement("div");
    head.className = "answer-pieces-turn-head";
    const collectedCount = turn.pieces.filter((p) => p.collected).length;
    const droppedCount   = turn.pieces.filter((p) => p.survived === false).length;
    const query = turn.query ? `: ${turn.query}` : "";
    head.innerHTML =
      `<span class="turn-number">Turn ${turn.turnNumber}</span>` +
      `<span class="turn-query" title="${escapeHtml(turn.query)}">${escapeHtml((query).slice(0, 100))}${(turn.query && turn.query.length > 100) ? "…" : ""}</span>` +
      `<span class="turn-progress"><b>${collectedCount} / ${turn.pieces.length}</b> collected` +
      (droppedCount > 0 ? ` · <span class="dropped-count">${droppedCount} dropped by ${escapeHtml(methodLbl.toLowerCase())}</span>` : "") +
      `</span>`;
    group.appendChild(head);

    for (const p of turn.pieces) {
      // Determine per-piece state label from the specific event that dropped
      // it (if any), not the session's latest method — otherwise a piece
      // dropped by TRUNCATION looks like it was dropped by the current
      // mode. Each piece now has a precise culprit.
      const dropMethod = (p.droppedByEvent && (p.droppedByEvent.data || {}).compaction_method)
        || turn.method;
      const dropMethodLbl = methodLabel(dropMethod);

      let stateClass, stateLabel;
      if (!p.collected)              { stateClass = "pending";   stateLabel = "waiting for tool"; }
      else if (p.survived === false) { stateClass = "dropped";   stateLabel = `DROPPED BY ${dropMethodLbl}`; }
      else if (p.survived === true)  { stateClass = "survived";  stateLabel = "still in context"; }
      else                           { stateClass = "collected"; stateLabel = "collected"; }

      const row = document.createElement("div");
      row.className = `answer-piece ${stateClass}`;
      const displayValue = p.collected_value || (p.collected ? "" : "(pending)");
      row.innerHTML =
        `<i class="ap-dot ${stateClass}"></i>` +
        `<span class="ap-label">${escapeHtml(p.label)}</span>` +
        `<span class="ap-value">${escapeHtml(displayValue || "—")}</span>` +
        `<span class="ap-source">from <code>${escapeHtml(p.source_tool || "?")}</code></span>` +
        `<span class="ap-state method-${dropMethod || "off"}">${escapeHtml(stateLabel)}</span>`;
      group.appendChild(row);

      // When the piece was dropped, name the specific compaction event and
      // offer a "View diff" link that opens the compacted context modal
      // anchored on that event — so the user can see exactly where the
      // value used to be.
      if (p.survived === false && p.droppedByEvent) {
        const hint = document.createElement("div");
        hint.className = "ap-missing-hint";
        const c = p.droppedByEvent;
        const when = formatTime(c.timestamp);
        const title = c.title || c.stage;
        hint.innerHTML =
          `value <code>${escapeHtml(p.collected_value || "")}</code> was removed by ` +
          `<b>${escapeHtml(title)}</b> at ${escapeHtml(when)}. `;
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "ap-diff-btn";
        btn.textContent = "View compacted context";
        btn.addEventListener("click", () => {
          const method = (c.data || {}).compaction_method || "—";
          const n = (c.data || {}).compacted_messages_full?.length || 0;
          openContextModal(
            "Compacted context (value dropped here)",
            `method=${method} · ${n} messages · ${title}`,
            c.data.compacted_messages_full,
            p.collected_value,
          );
        });
        hint.appendChild(btn);
        group.appendChild(hint);
      } else if (p.survived === false && !p.droppedByEvent) {
        const hint = document.createElement("div");
        hint.className = "ap-missing-hint";
        hint.textContent = `value "${p.collected_value || ""}" not found in the current context snapshot.`;
        group.appendChild(hint);
      }
    }

    const preview = turn.pieces.map((p) => p.collected_value || p.example || "?").join(" · ");
    const previewEl = document.createElement("div");
    previewEl.className = "answer-pieces-preview";
    previewEl.innerHTML = `<span class="ap-preview-label">Assembled answer:</span> <code>${escapeHtml(preview)}</code>`;
    group.appendChild(previewEl);

    answerPiecesListEl.appendChild(group);
  });
}

function renderSparkline(events) {
  const pts = [];
  for (const e of events) {
    if (e.source === "ce-manager" && e.stage === "token_count") {
      pts.push({ t: (e.data || {}).tokens_in ?? 0, mask: false, overflow: false });
    } else if (e.source === "ce-manager" && e.stage === "mask_applied" && e.status === "success") {
      if (pts.length) pts[pts.length - 1].mask = true;
    } else if (e.source === "ce-manager" && e.stage === "context_overflow") {
      if (pts.length) pts[pts.length - 1].overflow = true;
    }
  }

  const W = 800, H = 80, PAD = 4;
  sparklineEl.innerHTML = "";
  if (pts.length < 2) {
    const txt = document.createElementNS("http://www.w3.org/2000/svg", "text");
    txt.setAttribute("x", W / 2);
    txt.setAttribute("y", H / 2);
    txt.setAttribute("text-anchor", "middle");
    txt.setAttribute("fill", "#5d6a75");
    txt.setAttribute("font-size", "14");
    txt.textContent = "Waiting for more events...";
    sparklineEl.appendChild(txt);
    return;
  }
  const maxT = Math.max(CONTEXT_MAX_TOKENS, ...pts.map((p) => p.t));

  // Threshold lines
  for (const pct of [0.5, 0.75, 1.0]) {
    const y = PAD + (1 - pct * CONTEXT_MAX_TOKENS / maxT) * (H - 2 * PAD);
    const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
    line.setAttribute("x1", 0); line.setAttribute("x2", W);
    line.setAttribute("y1", y); line.setAttribute("y2", y);
    line.setAttribute("stroke", pct === 1 ? "#a32735" : pct === 0.75 ? "#d97a2c" : "#d9a72c");
    line.setAttribute("stroke-dasharray", "4 4"); line.setAttribute("opacity", "0.55");
    sparklineEl.appendChild(line);
  }

  // Token line
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  const step = (W - 2 * PAD) / (pts.length - 1);
  let d = "";
  pts.forEach((p, i) => {
    const x = PAD + i * step;
    const y = PAD + (1 - p.t / maxT) * (H - 2 * PAD);
    d += (i === 0 ? `M ${x} ${y}` : ` L ${x} ${y}`);
  });
  path.setAttribute("d", d);
  path.setAttribute("fill", "none");
  path.setAttribute("stroke", "#1c4e78");
  path.setAttribute("stroke-width", "2");
  sparklineEl.appendChild(path);

  // Markers
  pts.forEach((p, i) => {
    if (!p.mask && !p.overflow) return;
    const x = PAD + i * step;
    const y = PAD + (1 - p.t / maxT) * (H - 2 * PAD);
    const c = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    c.setAttribute("cx", x); c.setAttribute("cy", y); c.setAttribute("r", "4");
    c.setAttribute("fill", p.overflow ? "#a32735" : "#1d7d58");
    sparklineEl.appendChild(c);
  });
}

// Small helper — draw a single-series bar chart onto an <svg>.
// `bars` is an array of { value: number, color: string, title: string }.
// One <rect> per bar; each call (watsonx OR CE compactor) is its own bar,
// placed chronologically. Heights share the same y-scale.
function drawBarSvg(svgEl, bars, { height = 60, fmt, emptyMsg }) {
  if (!svgEl) return;
  const W = 800, H = height, PAD = 4;
  svgEl.innerHTML = "";
  if (!bars.length) {
    const txt = document.createElementNS("http://www.w3.org/2000/svg", "text");
    txt.setAttribute("x", W / 2); txt.setAttribute("y", H / 2);
    txt.setAttribute("text-anchor", "middle");
    txt.setAttribute("fill", "#9ca5ae"); txt.setAttribute("font-size", "12");
    txt.textContent = emptyMsg || "Waiting for LLM calls...";
    svgEl.appendChild(txt);
    return;
  }
  const maxV = Math.max(...bars.map(b => b.value || 0), 1e-9);
  const slotW = (W - 2 * PAD) / bars.length;
  const barW = Math.max(1, slotW - 1);
  bars.forEach((b, i) => {
    const v = b.value || 0;
    const h = Math.max(v > 0 ? 1 : 0, (v / maxV) * (H - 2 * PAD));
    const x = PAD + i * slotW;
    const y = H - PAD - h;
    const rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
    rect.setAttribute("x", x); rect.setAttribute("y", y);
    rect.setAttribute("width", barW); rect.setAttribute("height", h);
    rect.setAttribute("fill", b.color);
    const title = document.createElementNS("http://www.w3.org/2000/svg", "title");
    title.textContent = `#${i + 1} · ${b.title || ""}${fmt ? ` · ${fmt(v)}` : ""}`;
    rect.appendChild(title);
    svgEl.appendChild(rect);
  });
}

const _COST_WATSONX_COLOR  = "#1c4e78";
const _COST_CE_COLOR       = "#d97706";
const _LAT_WATSONX_COLOR   = "#0d6b49";
const _LAT_CE_COLOR        = "#d97706";

function renderCostLatencySparklines(events) {
  // Walk events chronologically. Each watsonx_call/success → one watsonx
  // bar. Each mask_applied/success → one CE-compactor bar. Bars are
  // interleaved in event order — no stacking.
  const costBars = [];
  const latencyBars = [];
  let totalWatsonxCost = 0, totalWatsonxLatency = 0;
  let totalCeCost = 0, totalCeLatency = 0;

  for (const e of events) {
    if (e.source !== "ce-manager") continue;
    const d = e.data || {};
    if (e.stage === "watsonx_call" && e.status === "success") {
      const c = typeof d.cost_usd === "number" ? d.cost_usd : 0;
      const l = typeof d.latency_ms === "number" ? d.latency_ms : 0;
      costBars.push({ value: c, color: _COST_WATSONX_COLOR, title: "watsonx call" });
      latencyBars.push({ value: l, color: _LAT_WATSONX_COLOR, title: "watsonx call" });
      totalWatsonxCost += c;
      totalWatsonxLatency += l;
    } else if (e.stage === "mask_applied" && e.status === "success") {
      const c = typeof d.ce_cost_usd === "number" ? d.ce_cost_usd : 0;
      const l = typeof d.ce_latency_ms === "number" ? d.ce_latency_ms :
                (typeof d.total_latency_ms === "number" ? d.total_latency_ms : 0);
      if (c > 0 || l > 0) {
        costBars.push({ value: c, color: _COST_CE_COLOR, title: "CE compactor" });
        latencyBars.push({ value: l, color: _LAT_CE_COLOR, title: "CE compactor" });
        totalCeCost += c;
        totalCeLatency += l;
      }
    }
  }

  drawBarSvg(costSparklineEl, costBars, {
    fmt: (v) => formatCost(v),
    emptyMsg: "Waiting for LLM calls...",
  });
  drawBarSvg(latencySparklineEl, latencyBars, {
    fmt: (v) => formatLatency(v),
    emptyMsg: "Waiting for LLM calls...",
  });

  const totalCost = totalWatsonxCost + totalCeCost;
  const totalLatency = totalWatsonxLatency + totalCeLatency;
  if (costTotalEl) {
    costTotalEl.textContent = formatCost(totalCost);
    costTotalEl.title = `watsonx ${formatCost(totalWatsonxCost)} + CE ${formatCost(totalCeCost)}`;
  }
  if (latencyTotalEl) {
    latencyTotalEl.textContent = formatLatency(totalLatency);
    latencyTotalEl.title = `watsonx ${formatLatency(totalWatsonxLatency)} + CE ${formatLatency(totalCeLatency)}`;
  }
  const costWx = document.getElementById("costTotalWatsonx");
  const costCe = document.getElementById("costTotalCe");
  if (costWx) costWx.textContent = formatCost(totalWatsonxCost);
  if (costCe) costCe.textContent = formatCost(totalCeCost);
  const latWx = document.getElementById("latencyTotalWatsonx");
  const latCe = document.getElementById("latencyTotalCe");
  if (latWx) latWx.textContent = formatLatency(totalWatsonxLatency);
  if (latCe) latCe.textContent = formatLatency(totalCeLatency);
}

// ---------- session render ----------

function computeSummary(events) {
  if (!events.length) return { text: "Waiting for events", className: "neutral" };
  const overflow = events.some((e) => e.source === "ce-manager" && e.stage === "context_overflow");
  const masked = events.filter((e) => e.source === "ce-manager" && e.stage === "mask_applied" && e.status === "success").length;
  const replies = events.filter((e) => e.stage === "assistant_reply").length;
  if (overflow && masked === 0) {
    return { text: "Context window exceeded — masker did not run (CE_MODE=off).", className: "blocked" };
  }
  if (masked > 0 && replies >= 2) {
    return { text: `Context engineering handled ${masked} compaction(s); both user questions answered.`, className: "success" };
  }
  if (replies >= 1) {
    return { text: "Session in progress. " + (masked > 0 ? `Masker fired ${masked} time(s).` : "No compactions yet."), className: "neutral" };
  }
  return { text: "Session running...", className: "neutral" };
}

// Track which event IDs have already been rendered into the timeline, per
// session. Clearing this set triggers a full rebuild on the next call.
const _renderedTimelineIds = new Map(); // sessionId -> Set<eventId>

function _eventKey(evt) {
  // Server payloads have no explicit id; use the combination of timestamp +
  // stage + status + source as a stable key — events from the observer are
  // append-only in time order.
  return `${evt.timestamp}|${evt.source}|${evt.stage}|${evt.status}`;
}

function renderSession(session) {
  const sessionChanged = !currentSession || currentSession.id !== session.id;
  currentSession = session;
  sessionTitleEl.textContent = session.title || `CE Demo ${session.id}`;
  sessionMetaEl.textContent = `${session.events?.length ?? 0} events · updated ${formatTime(session.updated_at || session.updatedAt)}`;

  const events = session.events || [];
  renderStatStrip(events);
  updateContextMeter(events);
  renderSparkline(events);
  renderCostLatencySparklines(events);
  renderAnswerPieces(events);
  renderConversation(events);

  // Timeline: incremental append. Rebuild columns only on session switch;
  // otherwise append only the events we haven't seen yet.
  let rendered = _renderedTimelineIds.get(session.id);
  if (sessionChanged || !rendered) {
    rendered = new Set();
    _renderedTimelineIds.set(session.id, rendered);
    createTimelineColumns();
  }
  for (const evt of events) {
    const k = _eventKey(evt);
    if (rendered.has(k)) continue;
    appendEventToTimeline(evt);
    rendered.add(k);
  }
}

function renderStatStrip(events) {
  const banner = summaryBannerEl;
  if (!events.length) {
    banner.className = "summary-banner neutral stat-strip";
    banner.textContent = "Waiting for events…";
    return;
  }

  const overflow = events.find((e) => e.source === "ce-manager" && e.stage === "context_overflow");
  const masked = events.filter(
    (e) => e.source === "ce-manager"
      && (e.stage === "mask_applied" || e.stage === "summary_applied" || e.stage === "truncation_applied")
      && e.status === "success",
  ).length;
  const replies = events.filter((e) => e.stage === "assistant_reply").length;
  const turns = events.filter((e) => e.stage === "user_turn").length;
  let latestTokens = 0;
  let totalCostUsd = 0;
  let totalLatencyMs = 0;
  for (const e of events) {
    if (e.source === "ce-manager" && e.stage === "token_count") {
      latestTokens = (e.data || {}).tokens_in ?? latestTokens;
    }
    if (e.source === "ce-manager" && e.stage === "watsonx_call" && e.status === "success") {
      totalCostUsd += Number((e.data || {}).cost_usd || 0);
      totalLatencyMs += Number((e.data || {}).latency_ms || 0);
    }
  }
  const firstTs = events[0]?.timestamp;
  const lastTs = events[events.length - 1]?.timestamp;
  const wallMs = (firstTs && lastTs)
    ? Math.max(0, new Date(lastTs).getTime() - new Date(firstTs).getTime())
    : 0;

  const cls = overflow && masked === 0 ? "blocked" : (replies >= 2 && !overflow ? "success" : "neutral");
  banner.className = `summary-banner stat-strip ${cls}`;
  banner.innerHTML =
    `<div class="strip-stat"><span class="strip-k">Turns</span><span class="strip-v">${turns} · ${replies} replied</span></div>` +
    `<div class="strip-stat"><span class="strip-k">Tokens</span><span class="strip-v">${formatInt(latestTokens)} / ${formatInt(CONTEXT_MAX_TOKENS)}</span></div>` +
    `<div class="strip-stat"><span class="strip-k">Compactions</span><span class="strip-v">${masked}</span></div>` +
    `<div class="strip-stat"><span class="strip-k">Wall time</span><span class="strip-v">${formatLatency(wallMs)}</span></div>` +
    `<div class="strip-stat"><span class="strip-k">Watsonx cost</span><span class="strip-v">${formatCost(totalCostUsd)}</span></div>` +
    `<div class="strip-stat"><span class="strip-k">Overflow</span><span class="strip-v ${overflow ? 'bad' : 'ok'}">${overflow ? 'YES' : 'no'}</span></div>`;
}

// ---------- session loading + SSE ----------

async function selectSession(sessionId) {
  selectedSessionId = sessionId;
  const resp = await fetch(`/api/sessions/${sessionId}`);
  if (!resp.ok) return;
  const session = await resp.json();
  renderSession(session);
  connectStream(sessionId);
  for (const item of document.querySelectorAll(".session-item")) {
    item.classList.toggle("active", item.dataset.sessionId === sessionId);
  }
}

function connectStream(sessionId) {
  if (eventSource) { eventSource.close(); eventSource = null; }
  eventSource = new EventSource(`/api/sessions/${sessionId}/stream`);
  liveBadgeEl.classList.remove("hidden");
  eventSource.addEventListener("message", (event) => {
    const evt = JSON.parse(event.data);
    if (!currentSession || currentSession.id !== sessionId) return;
    currentSession.events.push(evt);
    currentSession.updated_at = evt.timestamp;
    renderSession(currentSession);
  });
  eventSource.onerror = () => liveBadgeEl.classList.add("hidden");
}

async function loadSessions() {
  const resp = await fetch("/api/sessions");
  if (!resp.ok) return;
  const sessions = await resp.json();
  if (!sessions.length) {
    sessionListEl.className = "session-list empty-state";
    sessionListEl.textContent = "Waiting for demo sessions...";
    return;
  }
  sessionListEl.className = "session-list";
  sessionListEl.innerHTML = "";
  for (const s of sessions) {
    const item = document.getElementById("sessionItemTemplate").content.firstElementChild.cloneNode(true);
    item.dataset.sessionId = s.id;
    item.querySelector(".session-title").textContent = s.title || `CE Demo ${s.id}`;
    item.querySelector(".session-meta").textContent = `${s.event_count ?? 0} events · updated ${formatTime(s.updated_at || s.updatedAt)}`;
    item.addEventListener("click", () => selectSession(s.id));
    if (s.id === selectedSessionId) item.classList.add("active");
    sessionListEl.appendChild(item);
  }
  if (!selectedSessionId && sessions[0]) selectSession(sessions[0].id);
}

createTimelineColumns();
loadSessions();
setInterval(loadSessions, 5000);
