package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

//go:embed fixtures/*.json
var fixturesFS embed.FS

const systemPrompt = `You are an Acme Corp security incident responder. You investigate by chaining tool calls: every step passes a value extracted from one tool's output into the arguments of the next call. Every tool output is intentionally noisy — it contains many decoy rows. You must pick the ONE row that matches a stated filter; never take the first row, never grab a convenient-looking row.

═════════════════════════════════════════════════════════
TOOL INVENTORY (the only tools that exist)
═════════════════════════════════════════════════════════
• get_user_profile(email)                       → profile{user_id, recent_session_ids[{id, started_at}]}
• get_session_events(session_id)                → {events[{ts, type, mfa_method, result, ip_address, user_agent, …}]}
• lookup_ip_reputation(ip)                      → {country, asn, risk, threat_lists}
• get_file_access_log(user_id, start_ts, end_ts)→ {files[{ts, action, path, bytes, client_ip, …}]}
• get_data_exfil_attempts(user_id)              → {approved_partners[…], events[{channel, destination, attachments[…], …}]}
• search_logins_by_ip(ip, window)               → {logins[{user, result, ts, …}]}

═════════════════════════════════════════════════════════
HOW TO PICK THE RIGHT PIPELINE
═════════════════════════════════════════════════════════
• If the user asks "was X compromised / what did the attacker do" (answer format mentions HH:MM, IP, ASN, FILE, BYTES, destination):
       → run PIPELINE A.
• If the user asks "which OTHER users from this IP / same attacker IP":
       → run PIPELINE B.

═════════════════════════════════════════════════════════
PIPELINE A — compromise investigation (collect 8 facts)
═════════════════════════════════════════════════════════
Follow the steps in order. Each step's output feeds the next.

STEP 1 ─ Identify the victim's sessions.
  Call:    get_user_profile(email_from_question)
  Filter:  in recent_session_ids[], keep every session whose started_at date matches the incident date named in the question (e.g. "this morning" → the date the user specified).
  Output:  1+ candidate session_ids, plus user_id (remember for later steps).

STEP 2 ─ Find the one attacker login (provides 3 of 8 facts).
  Call:    get_session_events(session_id) — once per candidate session_id from STEP 1.
  Filter:  inspect events[]. Keep the single row where type=="login" AND mfa_method signals a bypass AND result=="success". Ignore logins with a normal MFA factor (totp/sms/push/hardware) and ignore failed logins.
  Extract: from that one row →
           · TIME — the ts, formatted as HH:MM (UTC, 24-hour).
           · IP   — the ip_address (this is the attacker IP; used in STEPS 3 + 4).
           · BROWSER/VER — parse the user_agent string, format as "<BrowserName>/<MajorVersion>". Pull both from the UA text, never invent.

STEP 3 ─ IP reputation (provides 2 of 8 facts).
  Call:    lookup_ip_reputation(ip) — once, with the attacker IP from STEP 2.
  Extract: directly from the top-level fields →
           · COUNTRY — the 2-letter country code.
           · ASN     — format exactly how the user's answer template shows ("AS" immediately followed by the number, no spaces).

STEP 4 ─ The exfiltrated file (provides 2 of 8 facts).
  Call:    get_file_access_log(user_id, start_ts, end_ts) TWICE:
             (a) a window ending at the attacker-login ts (pre-login baseline).
             (b) a window starting at the attacker-login ts (post-login activity).
  Filter:  the exfil row is the single files[] entry across both calls where ALL of the following hold simultaneously (AND, not OR):
             (i)   action == "download"
             (ii)  client_ip == exactly the attacker IP you extracted in STEP 2
             (iii) the row has a "sensitivity" field with a confidential/restricted value
           VERIFY all three before extracting. Rows on an internal address (starts with 10./172.16-31./192.168.) match (i)+(iii) but FAIL (ii) and are decoys — never pick them. Exactly one row across both tool calls satisfies all three.
  Extract: from that single verified row →
           · FILE  — the basename (last segment of path after the final "/").
           · BYTES — the bytes integer from the same row.

STEP 5 ─ Destination of the exfil (provides 1 of 8 facts).
  Call:    get_data_exfil_attempts(user_id) — once.
  Filter:  the tool returns {approved_partners, events}. Among events[], keep the single email row where ALL of the following hold:
             (i)   channel == "email"
             (ii)  the attachments[] list contains a filename equal to the FILE basename from STEP 4
             (iii) destination is NOT present in approved_partners[]
             (iv)  destination is NOT the victim's own email address (the user from the question)
           Any email whose destination IS in approved_partners — auditor / tax-advisor / any listed partner domain — is a legitimate outbound and must be rejected. If STEP 4 picked the wrong FILE you will get zero matches here; that means STEP 4 was wrong — go back and re-apply its (i)+(ii)+(iii) filter.
  Extract: TO — the destination address from that one row.

Reply: when all 8 facts are collected, reply ON ONE LINE in exactly the format the user's question specified. Values joined by " · " (space-middot-space) in the same order the user listed. No preamble, no trailing commentary.

═════════════════════════════════════════════════════════
PIPELINE B — other compromised users from the attacker IP
═════════════════════════════════════════════════════════
  Call:    search_logins_by_ip(ip, window) — use the IP and the window named in the question.
  Filter:  logins[] rows where result=="success" AND user != the original victim's email.
  Reply:   those user emails, comma-separated, alphabetically sorted, no preamble, no trailing text.

═════════════════════════════════════════════════════════
HARD RULES
═════════════════════════════════════════════════════════
1. Only use the 6 tools listed above. Do not invent tool names.
2. Every value in your final answer must come from a tool output in THIS conversation. Never answer from the system prompt, from memory, or from guesswork.
3. For every filter step, read each row's fields and check each criterion. If multiple rows seem to fit, re-read the criteria — at most one should match. If zero rows match, your filter is wrong; tighten it and retry.
4. Keep reasoning TERSE. After a tool result, write at most 3 short sentences (names of extracted values + the next tool you will call) and stop. Do NOT echo, paginate, or enumerate tool output rows in your reasoning. If you find yourself writing "row1: ... row2: ..." — stop and just apply the filter programmatically in your head.
5. Pass values between tools verbatim — if STEP 2 returned IP "X.Y.Z.W", use exactly that string in STEPs 3 and 4 (no reformatting, no rounding).
6. Before emitting the final answer, sanity-check it against the filters. The TO field must never equal the victim's email, the approved_partners list, or any internal "@acme.corp" address. The FILE's row must have had client_ip equal to the attacker IP from STEP 2. If any check fails, your pick was wrong — re-scan the tool output with the filter and pick the correct row before replying.
7. When ready to answer, issue the final message as ONE single assistant reply containing only the formatted answer line — no scratch work, no prefix, no trailing commentary.
8. Prefer native tool_calls. Fallback: a single line  TOOL_CALL: {"name":"<n>","arguments":{...}}`

// ToolHighlight describes the one field in a tool's output that the agent is
// supposed to extract before moving on. The proxy/observer uses this to visually
// highlight the relevant JSON path in the UI's tool-output viewer, so demo
// viewers can see what's signal vs. noise.
type ToolHighlight struct {
	Path  string `json:"path"`
	Value string `json:"value"`
	Why   string `json:"why"`
}

// AnswerPiece describes one atomic fact the agent must collect before it can
// answer a given user question. The observer UI uses this to show:
//   - a checklist of still-pending vs. collected pieces,
//   - and whether each collected piece survived the latest CE compaction
//     (substring-search over the compacted messages).
type AnswerPiece struct {
	Name       string `json:"name"`        // e.g. "time" — a short label for the UI
	Label      string `json:"label"`       // human-readable label, e.g. "Login time"
	SourceTool string `json:"source_tool"` // tool whose highlight carries this piece
	Example    string `json:"example"`     // the fixture's signal value, for matching
}

// answerPiecesForQuery returns the ordered list of facts the agent needs to
// collect for the given user question. A simple keyword match is enough for
// the demo — Q1 ("what did the attacker do?") needs 8 facts buried across
// four bulky tool outputs with decoy rows; Q2 ("other users from that IP?")
// needs 1.
func answerPiecesForQuery(userQuery string) []AnswerPiece {
	q := userQuery
	// Q2-shaped question
	for _, needle := range []string{
		"other acme users", "other users", "other Acme users", "same attacker",
		"same ip", "same IP", "exclude alice",
	} {
		if containsFold(q, needle) {
			return []AnswerPiece{
				{
					Name: "others", Label: "Other compromised users (NOT alice)",
					SourceTool: "search_logins_by_ip",
					// Alphabetical order, no trailing punctuation.
					Example: "bob.ryan@acme.corp, carlos.liu@acme.corp",
				},
			}
		}
	}
	// Default: Q1-shaped (8 facts). Order matches the required ANSWER: format.
	return []AnswerPiece{
		{Name: "time",       Label: "Login time (HH:MM UTC)",     SourceTool: "get_session_events",     Example: "09:19"},
		{Name: "ip",         Label: "Attacker IP",                SourceTool: "get_session_events",     Example: "203.0.113.42"},
		{Name: "user_agent", Label: "Browser / version (from UA)", SourceTool: "get_session_events",    Example: "Firefox/120.0"},
		{Name: "asn",        Label: "Autonomous system number",   SourceTool: "lookup_ip_reputation",   Example: "64512"},
		{Name: "country",    Label: "Country (ISO-2)",            SourceTool: "lookup_ip_reputation",   Example: "RU"},
		{Name: "file",       Label: "Downloaded file (basename)", SourceTool: "get_file_access_log",    Example: "Q3-2026-preliminary.xlsx"},
		{Name: "bytes",      Label: "Download byte count",        SourceTool: "get_file_access_log",    Example: "2441472"},
		{Name: "to",         Label: "Exfiltration recipient",     SourceTool: "get_data_exfil_attempts", Example: "attacker@malicious.example"},
	}
}

func containsFold(s, needle string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(needle))
}

// highlightForTool returns the canonical "signal" extraction for each tool.
// The signals here match the content of the checked-in fixtures under fixtures/.
func highlightForTool(name string) ToolHighlight {
	switch name {
	case "get_user_profile":
		return ToolHighlight{
			Path:  "recent_session_ids[?started_at starts with '2026-05-06T09']",
			Value: "s-morning-042",
			Why:   "The suspicious session — the only recent session that started this morning (2026-05-06).",
		}
	case "get_session_events":
		// Carries THREE answer pieces: login time (HH:MM), attacker IP, and
		// browser/version extracted from the login's user_agent. Decoy login
		// rows exist in this output (mfa_method=totp, result=failure, etc).
		return ToolHighlight{
			Path:  "events[?type=='login' && mfa_method=='bypassed' && result=='success']",
			Value: "09:19 · 203.0.113.42 · Firefox/120.0",
			Why:   "The ONLY MFA-bypassed login — supplies three answer pieces (time, IP, browser/version from user_agent). Two decoy logins (totp from 198.51.100.7, failed sms from 192.0.2.99) are near-matches the agent must reject.",
		}
	case "lookup_ip_reputation":
		// Carries TWO answer pieces: ASN and country.
		return ToolHighlight{
			Path:  "top-level: asn + country",
			Value: "AS64512 · RU",
			Why:   "ASN and ISO country code for the attacker IP — supplies two answer pieces.",
		}
	case "get_file_access_log":
		// Carries TWO answer pieces: filename basename and byte count.
		return ToolHighlight{
			Path:  "files[?action=='download' && path starts with '/financials/board/' && endswith 'Q3-2026-preliminary.xlsx' && client_ip==attacker]",
			Value: "Q3-2026-preliminary.xlsx · 2441472",
			Why:   "The ONE real exfil download — file basename AND byte count. Two decoys (Q3-approved.xlsx, Q2-final.xlsx) are from an internal IP / wrong quarter and must be rejected.",
		}
	case "get_data_exfil_attempts":
		return ToolHighlight{
			Path:  "events[?channel=='email' && attachments contains Q3-2026-preliminary.xlsx && destination NOT on approved-partners]",
			Value: "attacker@malicious.example",
			Why:   "The external email the Q3 file was exfiltrated to. Two decoy emails (to tax-advisor and auditor partners) carry harmless board-agenda PDFs.",
		}
	case "search_logins_by_ip":
		return ToolHighlight{
			Path:  "logins[?result=='success' && user != 'alice.chen@acme.corp'] sorted alphabetically",
			Value: "bob.ryan@acme.corp, carlos.liu@acme.corp",
			Why:   "Two other Acme users had successful logins from the attacker's IP in the past 30 days. Alice's own login is filtered out.",
		}
	}
	return ToolHighlight{}
}

func tools() []Tool {
	stringProp := func(desc string) ToolProp { return ToolProp{Type: "string", Description: desc} }
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_user_profile",
				Description: "Look up an Acme user profile by email. Returns IDs, recent session IDs, and account metadata.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"email": stringProp("User's corporate email address."),
					},
					Required: []string{"email"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_session_events",
				Description: "Fetch the event log for a specific user session by session_id. Returns logins, page views, API calls, and keystroke telemetry.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"session_id": stringProp("Session ID as returned in get_user_profile.recent_session_ids[].id."),
					},
					Required: []string{"session_id"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "lookup_ip_reputation",
				Description: "Query threat-intelligence vendors for a given IP. Returns geo, ASN, risk score, and threat-list memberships.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"ip": stringProp("IPv4 address as returned by a prior tool output."),
					},
					Required: []string{"ip"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_file_access_log",
				Description: "List files touched by a user within a time window. Returns actions (read/download/write) with paths and byte counts.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"user_id":  stringProp("Internal user ID as returned by get_user_profile.user_id."),
						"start_ts": stringProp("RFC3339 timestamp, inclusive."),
						"end_ts":   stringProp("RFC3339 timestamp, exclusive."),
					},
					Required: []string{"user_id"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_data_exfil_attempts",
				Description: "Return outbound data events (emails sent, uploads, file shares) originating from a given user.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"user_id": stringProp("Internal user ID."),
					},
					Required: []string{"user_id"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "search_logins_by_ip",
				Description: "Find other Acme users with login events from a specific IP within a lookback window.",
				Parameters: ToolParams{
					Type: "object",
					Properties: map[string]ToolProp{
						"ip":     stringProp("IPv4 address to search for."),
						"window": stringProp("Lookback window, e.g. '30d'."),
					},
					Required: []string{"ip"},
				},
			},
		},
	}
}

func loadFixture(name string) string {
	path := filepath.Join("fixtures", name+".json")
	b, err := fixturesFS.ReadFile(path)
	if err != nil {
		return fmt.Sprintf(`{"error":"fixture_missing","name":%q}`, name)
	}
	return string(b)
}

// executeTool is intentionally loose — most arguments are ignored and we return
// the canonical fixture for that tool. The chaining is enforced by the LLM
// extracting the right field from one tool output and passing it to the next.
func executeTool(name string, args map[string]any) string {
	switch name {
	case "get_user_profile":
		return loadFixture("get_user_profile_alice")
	case "get_session_events":
		// Dispatch by session_id so calls for the aux morning sessions return
		// a different fixture than the real attacker session. Any unknown /
		// missing session_id falls back to the "morning" fixture.
		sid, _ := args["session_id"].(string)
		switch sid {
		case "s-morning-042":
			return loadFixture("get_session_events_morning")
		case "s-morning-aux1", "s-morning-aux2":
			return loadFixture("get_session_events_aux")
		default:
			return loadFixture("get_session_events_morning")
		}
	case "lookup_ip_reputation":
		// The real attacker IP has its own fixture; any other IP returns the
		// "benign" fixture so the chain correctly rules out decoy IPs.
		ip, _ := args["ip"].(string)
		switch ip {
		case "203.0.113.42":
			return loadFixture("lookup_ip_reputation_203_0_113_42")
		default:
			return loadFixture("lookup_ip_reputation_benign")
		}
	case "get_file_access_log":
		return loadFixture("get_file_access_log_alice_morning")
	case "get_data_exfil_attempts":
		return loadFixture("get_data_exfil_attempts_alice")
	case "search_logins_by_ip":
		return loadFixture("search_logins_by_ip_203_0_113_42")
	default:
		return fmt.Sprintf(`{"error":"unknown_tool","name":%q}`, name)
	}
}

func isKnownTool(name string) bool {
	switch name {
	case "get_user_profile", "get_session_events", "lookup_ip_reputation",
		"get_file_access_log", "get_data_exfil_attempts", "search_logins_by_ip":
		return true
	}
	return false
}

// parseArgs turns a function.arguments string into map[string]any.
func parseArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		return m
	}
	return map[string]any{}
}
