const stageConfig = [
  { key: "user", label: "User" },
  { key: "finance-agent", label: "Finance Agent" },
  { key: "sparc", label: "SPARC" },
  { key: "finance-backend", label: "Finance Backend" },
  { key: "ibac", label: "IBAC" },
  { key: "network", label: "Network" },
];

let selectedSessionId = null;
let eventSource = null;
let currentSession = null;

const sessionListEl = document.getElementById("sessionList");
const sessionTitleEl = document.getElementById("sessionTitle");
const sessionMetaEl = document.getElementById("sessionMeta");
const summaryBannerEl = document.getElementById("summaryBanner");
const timelineColumnsEl = document.getElementById("timelineColumns");
const logGroupsEl = document.getElementById("logGroups");
const liveBadgeEl = document.getElementById("liveBadge");
const conversationFeedEl = document.getElementById("conversationFeed");

document.getElementById("refreshSessions").addEventListener("click", loadSessions);

function formatTime(value) {
  if (!value) return "";
  const date = new Date(value);
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function formatSessionMeta(session) {
  if (!session) return "";
  return `${session.events?.length ?? session.event_count ?? 0} events · updated ${formatTime(session.updated_at || session.updatedAt)}`;
}

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

function eventLane(evt) {
  if (evt.stage === "user_turn") return "user";
  if (evt.source === "finance-agent") return "finance-agent";
  if (stageConfig.some((stage) => stage.key === evt.source)) return evt.source;
  return "network";
}

function shortenMiddle(value, maxLength = 46) {
  if (!value || value.length <= maxLength) return value || "";
  const head = Math.ceil((maxLength - 3) / 2);
  const tail = Math.floor((maxLength - 3) / 2);
  return `${value.slice(0, head)}...${value.slice(value.length - tail)}`;
}

function compactValue(value) {
  if (typeof value === "string") {
    return shortenMiddle(value.replace(/\s+/g, " ").trim(), 70);
  }
  if (typeof value === "number") {
    return String(value);
  }
  if (typeof value === "boolean") {
    return value ? "true" : "false";
  }
  if (Array.isArray(value)) {
    if (!value.length) return "[]";
    return `[${value.length} items]`;
  }
  if (value && typeof value === "object") {
    return "{...}";
  }
  return "";
}

function conciseSummary(evt) {
  const data = evt.data || {};

  if (evt.source === "sparc" || evt.stage.startsWith("sparc")) {
    const score = data.overall_avg_score ?? data.score;
    const issues = Array.isArray(data.issues) ? data.issues : [];
    const metric = issues[0]?.metric_name || issues[0]?.issue_type;
    const parts = [];
    if (data.tool_name) parts.push(`tool ${data.tool_name}`);
    if (data.decision) parts.push(`decision ${String(data.decision).toUpperCase()}`);
    if (score !== undefined) parts.push(`score ${score}`);
    if (metric) parts.push(`issue ${metric}`);
    return parts.join(" · ");
  }

  if (evt.source === "ibac") {
    const parts = [];
    if (data.method) parts.push(data.method);
    if (data.authority) parts.push(shortenMiddle(data.authority, 42));
    if (data.decision) parts.push(String(data.decision).toUpperCase());
    return parts.join(" · ");
  }

  if (evt.stage === "model_proposal") {
    return data.tool_name ? `tool ${data.tool_name}` : "";
  }

  if (evt.stage === "network_attempt") {
    const parts = [];
    if (data.tool) parts.push(data.tool);
    if (data.arguments?.url) parts.push(shortenMiddle(data.arguments.url, 46));
    return parts.join(" · ");
  }

  if (evt.source === "finance-backend") {
    if (data.invoice_id) return `invoice ${data.invoice_id}`;
    if (data.transaction_id) return `transaction ${data.transaction_id}`;
  }

  const entries = Object.entries(data)
    .filter(([key]) => !["raw_pipeline_result", "issues", "message"].includes(key))
    .slice(0, 3)
    .map(([key, value]) => `${key}: ${compactValue(value)}`);

  return entries.join(" · ");
}

function sanitizeEventData(evt) {
  const data = evt.data || {};
  if (!Object.keys(data).length) return null;

  const clone = JSON.parse(JSON.stringify(data));
  if (clone.raw_pipeline_result) {
    const raw = clone.raw_pipeline_result;
    clone.raw_pipeline_result = {
      overall_avg_score: raw.overall_avg_score,
      overall_valid: raw.overall_valid,
      semantic: raw.semantic ? "[expanded in source payload]" : undefined,
      static: raw.static ? "[expanded in source payload]" : undefined,
    };
  }
  return clone;
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
  const previewEl = card.querySelector(".event-preview");
  if (preview) {
    previewEl.classList.remove("hidden");
    previewEl.textContent = preview;
  }

  const sanitizedData = sanitizeEventData(evt);
  const dataEl = card.querySelector(".event-data");
  const toggleEl = card.querySelector(".event-toggle");
  if (sanitizedData && Object.keys(sanitizedData).length > 0) {
    dataEl.textContent = JSON.stringify(sanitizedData, null, 2);
    toggleEl.classList.remove("hidden");
    toggleEl.addEventListener("click", () => {
      const isHidden = dataEl.classList.toggle("hidden");
      toggleEl.textContent = isHidden ? "View JSON" : "Hide JSON";
    });
  }

  column.appendChild(card);
}

function renderLogs(events) {
  const grouped = new Map();
  for (const evt of events) {
    if (!evt.raw_log) continue;
    if (!grouped.has(evt.source)) grouped.set(evt.source, []);
    grouped.get(evt.source).push(evt);
  }

  if (grouped.size === 0) {
    logGroupsEl.className = "log-groups empty-state";
    logGroupsEl.textContent = "No logs yet.";
    return;
  }

  logGroupsEl.className = "log-groups";
  logGroupsEl.innerHTML = "";
  for (const [source, entries] of grouped) {
    const group = document.createElement("section");
    group.className = "log-group";
    const title = document.createElement("h3");
    title.textContent = source;
    group.appendChild(title);

    for (const evt of entries.slice(-6)) {
      const log = document.createElement("pre");
      log.className = "log-entry";
      log.textContent = `[${formatTime(evt.timestamp)}] ${evt.raw_log}`;
      group.appendChild(log);
    }
    logGroupsEl.appendChild(group);
  }
}

function conversationItems(events) {
  const items = [];
  let lastAssistantReply = null;

  for (const evt of events) {
    const include =
      evt.stage === "user_turn" ||
      evt.stage === "assistant_reply" ||
      evt.stage === "model_proposal" ||
      (evt.stage === "sparc_result" && evt.status === "blocked") ||
      (evt.source === "finance-backend" && evt.status === "success") ||
      evt.stage === "network_attempt";

    if (!include) continue;

    if (evt.stage === "assistant_reply") {
      const key = `${evt.summary || evt.raw_log || ""}|${evt.timestamp || ""}`;
      if (lastAssistantReply === key) {
        continue;
      }
      lastAssistantReply = key;
    }

    items.push(evt);
  }

  return items;
}

function conversationEntry(evt) {
  if (evt.stage === "user_turn") {
    return {
      roleClass: "user",
      speaker: "User",
      kind: "User request",
      text: evt.summary || evt.raw_log || "",
    };
  }

  if (evt.stage === "assistant_reply") {
    return {
      roleClass: "agent",
      speaker: "Finance Agent",
      kind: "Agent reply",
      text: evt.summary || evt.raw_log || "",
    };
  }

  if (evt.stage === "model_proposal") {
    const toolName = evt.data?.tool_name || "tool";
    const args = evt.data?.arguments ? JSON.stringify(evt.data.arguments) : "";
    return {
      roleClass: "tool",
      speaker: "Finance Agent",
      kind: "Tool call",
      text: `${toolName}${args ? ` ${args}` : ""}`,
    };
  }

  if (evt.stage === "sparc_result" && evt.status === "blocked") {
    const toolName = evt.data?.tool_name || "tool";
    const issue = Array.isArray(evt.data?.issues) ? evt.data.issues[0] : null;
    const explanation =
      issue?.explanation ||
      issue?.metric_name ||
      evt.summary ||
      "SPARC blocked the proposed tool call.";
    return {
      roleClass: "guard",
      speaker: "SPARC",
      kind: "Blocked tool call",
      text: `${toolName} · ${explanation}`,
    };
  }

  if (evt.source === "finance-backend") {
    const pieces = [];
    if (evt.stage === "transaction_lookup") {
      pieces.push(`transaction ${evt.data?.transaction_id || ""}`.trim());
      if (evt.data?.amount !== undefined && evt.data?.currency) {
        pieces.push(`${evt.data.amount} ${evt.data.currency}`);
      }
    } else if (evt.stage === "customer_lookup") {
      pieces.push(`customer ${evt.data?.customer_id || ""}`.trim());
      if (evt.data?.name) {
        pieces.push(evt.data.name);
      }
    } else if (evt.stage === "refund_issue") {
      pieces.push(`refund ${evt.data?.transaction_id || ""}`.trim());
      if (evt.data?.amount !== undefined) {
        pieces.push(`amount ${evt.data.amount}`);
      }
      if (evt.data?.refund_reason) {
        pieces.push(`reason ${evt.data.refund_reason}`);
      }
    } else if (evt.stage === "invoice_lookup") {
      pieces.push(`invoice ${evt.data?.invoice_id || ""}`.trim());
      if (evt.data?.audit_url) {
        pieces.push(`embedded URL ${shortenMiddle(evt.data.audit_url, 52)}`);
      }
    }

    return {
      roleClass: "tool",
      speaker: "Finance Backend",
      kind: "Tool output",
      text: pieces.filter(Boolean).join(" · ") || evt.summary || "",
    };
  }

  if (evt.stage === "network_attempt") {
    const toolName = evt.data?.tool || "http_post";
    const url = evt.data?.arguments?.url || "";
    return {
      roleClass: evt.status === "blocked" ? "guard" : "tool",
      speaker: evt.status === "blocked" ? "IBAC" : "Finance Agent",
      kind: evt.status === "blocked" ? "Blocked tool output" : "Tool call",
      text: url ? `${toolName} ${shortenMiddle(url, 52)}` : evt.summary || "",
    };
  }

  return null;
}

function renderConversation(events) {
  const items = conversationItems(events);
  if (!items.length) {
    conversationFeedEl.className = "conversation-feed empty-state";
    conversationFeedEl.textContent = "No conversation yet.";
    return;
  }

  conversationFeedEl.className = "conversation-feed";
  conversationFeedEl.innerHTML = "";

  for (const evt of items) {
    const entry = conversationEntry(evt);
    if (!entry) continue;
    const item = document.getElementById("conversationItemTemplate").content.firstElementChild.cloneNode(true);
    item.classList.add(entry.roleClass);
    item.querySelector(".conversation-speaker").textContent = entry.speaker;
    item.querySelector(".conversation-time").textContent = formatTime(evt.timestamp);
    item.querySelector(".conversation-kind").textContent = entry.kind;
    item.querySelector(".conversation-text").textContent = entry.text;
    conversationFeedEl.appendChild(item);
  }
}

function computeSummary(events) {
  if (!events.length) {
    return { text: "Waiting for events", className: "neutral" };
  }

  const sparcBlocked = events.some((evt) => evt.source === "sparc" && evt.status === "blocked");
  const ibacBlocked = events.some((evt) => evt.source === "ibac" && evt.status === "blocked");
  const refundSucceeded = events.some((evt) => evt.source === "finance-agent" && evt.stage === "refund" && evt.status === "success");

  if (sparcBlocked && ibacBlocked && refundSucceeded) {
    return {
      text: "SPARC blocked the hallucinated transaction ID, the refund completed after clarification, and IBAC blocked the injected outbound POST.",
      className: "success",
    };
  }
  if (events.some((evt) => evt.status === "blocked")) {
    return {
      text: "Pipeline contains blocked actions. Review the conversation and timeline for the exact step.",
      className: "blocked",
    };
  }
  return {
    text: "Session in progress. Waiting for more events...",
    className: "neutral",
  };
}

function renderSession(session) {
  currentSession = session;
  sessionTitleEl.textContent = session.title || `Session ${session.id}`;
  sessionMetaEl.textContent = formatSessionMeta(session);

  const summary = computeSummary(session.events || []);
  summaryBannerEl.className = `summary-banner ${summary.className}`;
  summaryBannerEl.textContent = summary.text;

  renderConversation(session.events || []);
  createTimelineColumns();
  for (const evt of session.events || []) {
    appendEventToTimeline(evt);
  }
  renderLogs(session.events || []);
}

async function selectSession(sessionId) {
  selectedSessionId = sessionId;
  const response = await fetch(`/api/sessions/${sessionId}`);
  if (!response.ok) return;
  const session = await response.json();
  renderSession(session);
  connectStream(sessionId);

  for (const item of document.querySelectorAll(".session-item")) {
    item.classList.toggle("active", item.dataset.sessionId === sessionId);
  }
}

function connectStream(sessionId) {
  if (eventSource) {
    eventSource.close();
    eventSource = null;
  }

  eventSource = new EventSource(`/api/sessions/${sessionId}/stream`);
  liveBadgeEl.classList.remove("hidden");

  eventSource.addEventListener("message", (event) => {
    const evt = JSON.parse(event.data);
    if (!currentSession || currentSession.id !== sessionId) return;

    currentSession.events.push(evt);
    currentSession.updated_at = evt.timestamp;
    renderSession(currentSession);
  });

  eventSource.onerror = () => {
    liveBadgeEl.classList.add("hidden");
  };
}

async function loadSessions() {
  const response = await fetch("/api/sessions");
  if (!response.ok) return;
  const sessions = await response.json();

  if (!sessions.length) {
    sessionListEl.className = "session-list empty-state";
    sessionListEl.textContent = "Waiting for demo sessions...";
    return;
  }

  sessionListEl.className = "session-list";
  sessionListEl.innerHTML = "";

  for (const session of sessions) {
    const item = document.getElementById("sessionItemTemplate").content.firstElementChild.cloneNode(true);
    item.dataset.sessionId = session.id;
    item.querySelector(".session-title").textContent = session.title || `Session ${session.id}`;
    item.querySelector(".session-meta").textContent = formatSessionMeta(session);
    item.addEventListener("click", () => selectSession(session.id));
    if (session.id === selectedSessionId) {
      item.classList.add("active");
    }
    sessionListEl.appendChild(item);
  }

  if (!selectedSessionId && sessions[0]) {
    selectSession(sessions[0].id);
  }
}

createTimelineColumns();
loadSessions();
setInterval(loadSessions, 5000);
