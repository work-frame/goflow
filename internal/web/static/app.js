// GoFlow dashboard. Plain JavaScript, no libraries, served from the API's
// own origin under a strict Content-Security-Policy (see internal/web).
//
// Rules this file keeps:
//   - Text from the server (job titles above all) is only ever set with
//     textContent, never as HTML: a title like <img onerror=...> stays text.
//   - The session token lives in sessionStorage: it survives a refresh and
//     is gone when the tab closes. It is sent only in the Authorization
//     header, never in a URL. The event stream gets a 30-second ticket instead.
//   - PostgreSQL is the truth. Live events only update what the page shows;
//     after "ready", "resync", "resumed" or any reconnect, the page reloads
//     the list.
//   - A webhook job shows only its URL's host (hostOf), never the path,
//     the query (often a token), the body or anything about the signing
//     secret, which the dashboard never asks for. Attempts are shown as the
//     API gives them: errors already cut to 300 characters, without URLs.
//   - The dot has three states: live (stream open, events arriving),
//     paused (stream open, but the server says events stopped reaching it,
//     so the list may be stale) and offline/reconnecting (stream closed).
//     An open stream alone never shows "live".
"use strict";

const TOKEN_KEY = "goflow.token";
const API = "/api/v1";

// jobs holds the page's copy of each job by ID. deleted remembers IDs
// deleted while the page is open, so a late "changed" event can't bring
// one back.
const jobs = new Map();
const deleted = new Set();

// attempts holds the delivery attempts shown under webhook jobs whose
// "Attempts" button is open, by job ID.
const attempts = new Map();

let events = null;      // the open EventSource, if any
let retryDelay = 1000;  // reconnect backoff, doubling to 30 s
let retryTimer = null;

const $ = (id) => document.getElementById(id);

// ---------- API calls ----------

class Unauthorized extends Error {}

// api sends one request with the session token and returns the parsed
// JSON (or null for 204). A 401 means the session is gone: log out.
async function api(method, path, body, extraHeaders) {
  const headers = { Authorization: "Bearer " + sessionStorage.getItem(TOKEN_KEY) };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  Object.assign(headers, extraHeaders || {});
  const resp = await fetch(API + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 401) throw new Unauthorized();
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || "HTTP " + resp.status);
    err.status = resp.status;
    throw err;
  }
  return data;
}

// handle runs an action and shows its error, or logs out on 401.
async function handle(action, errorBox) {
  errorBox.hidden = true;
  try {
    await action();
  } catch (err) {
    if (err instanceof Unauthorized) {
      logout(false);
      return;
    }
    errorBox.textContent = err.message;
    errorBox.hidden = false;
  }
}

// ---------- keeping the copy right ----------

// apply keeps a job only if it is newer than the copy the page has.
// version goes up by 1 with every committed change, in commit order, so
// "higher version" means "newer", however late the event arrived.
function apply(job) {
  if (deleted.has(job.id)) return;
  const have = jobs.get(job.id);
  if (have && have.version >= job.version) return;
  jobs.set(job.id, job);
  // An open attempts list is out of date when its job changes.
  if (attempts.has(job.id)) loadAttempts(job.id);
}

// loadAll reads every job from PostgreSQL and merges it in. Jobs the page
// had before the load started but the database no longer has were deleted
// meanwhile. Jobs that arrived by event during the load are kept: they
// are newer than the snapshot.
async function loadAll() {
  const before = new Set(jobs.keys());
  const seen = new Set();
  let cursor = "";
  do {
    const q = new URLSearchParams({ limit: "200" });
    if (cursor) q.set("cursor", cursor);
    const page = await api("GET", "/jobs?" + q.toString());
    for (const job of page.jobs) {
      seen.add(job.id);
      apply(job);
    }
    cursor = page.next_cursor || "";
  } while (cursor);
  for (const id of before) {
    if (!seen.has(id)) jobs.delete(id);
  }
  render();
}

// ---------- live updates ----------

function setLive(state, text) {
  const el = $("live");
  el.className = "live " + state;
  el.textContent = text;
}

// connect gets a ticket and opens the event stream. Every way the stream
// can end leads back here after a backoff; "ready" always reloads the list.
async function connect() {
  clearTimeout(retryTimer);
  if (events) events.close();
  events = null;
  setLive("wait", "connecting");

  let ticket;
  try {
    ticket = (await api("POST", "/events/ticket")).ticket;
  } catch (err) {
    if (err instanceof Unauthorized) {
      logout(false);
      return;
    }
    // 503: no Redis on this server, or it's unreachable. Show what the
    // database has and try again later.
    await handle(loadAll, $("jobs-error"));
    return scheduleReconnect(err.status === 503 ? "live updates unavailable" : "offline");
  }

  const es = new EventSource(API + "/events?ticket=" + encodeURIComponent(ticket));
  events = es;
  es.onmessage = (e) => {
    let msg;
    try {
      msg = JSON.parse(e.data);
    } catch {
      return;
    }
    switch (msg.type) {
      case "ready":
        retryDelay = 1000;
        setLive("on", "live");
        handle(loadAll, $("jobs-error"));
        break;
      case "resync":
        handle(loadAll, $("jobs-error"));
        break;
      case "paused":
        // The stream is fine; the server's Redis subscription isn't. Keep
        // the stream open and wait for "resumed".
        setLive("paused", "paused: updates delayed");
        break;
      case "resumed":
        // Events sent during the pause are gone: reload from PostgreSQL.
        setLive("on", "live");
        handle(loadAll, $("jobs-error"));
        break;
      case "changed":
        apply(msg.job);
        render();
        break;
      case "deleted":
        deleted.add(msg.id);
        jobs.delete(msg.id);
        render();
        break;
    }
  };
  // EventSource would retry on its own, with the same, already used
  // ticket. Close it and reconnect with a new one instead.
  es.onerror = () => {
    if (events !== es) return;
    es.close();
    events = null;
    scheduleReconnect("reconnecting");
  };
}

function scheduleReconnect(label) {
  setLive("off", label + " (retry in " + Math.round(retryDelay / 1000) + " s)");
  retryTimer = setTimeout(connect, retryDelay);
  retryDelay = Math.min(retryDelay * 2, 30000);
}

// ---------- drawing ----------

function cell(text, className) {
  const td = document.createElement("td");
  td.textContent = text;
  if (className) td.className = className;
  return td;
}

function button(label, onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "small";
  b.textContent = label;
  b.addEventListener("click", onClick);
  return b;
}

// hostOf is the only thing the page shows of a webhook's URL.
function hostOf(url) {
  try {
    return new URL(url).hostname;
  } catch {
    return "webhook";
  }
}

// attemptsRow is the row under a webhook job that lists its attempts.
function attemptsRow(id) {
  const tr = document.createElement("tr");
  tr.className = "attempts";
  const td = cell("", "attempts");
  td.colSpan = 6;
  const list = attempts.get(id);
  if (list === null) {
    td.textContent = "Loading attempts…";
  } else if (list.length === 0) {
    td.textContent = "No attempts yet.";
  } else {
    const table = document.createElement("table");
    const head = document.createElement("tr");
    for (const h of ["#", "Started", "Took", "Status", "Outcome", "Error"]) {
      const th = document.createElement("th");
      th.textContent = h;
      head.append(th);
    }
    table.append(head);
    for (const a of list) {
      const row = document.createElement("tr");
      row.append(
        cell(String(a.attempt)),
        cell(new Date(a.started_at).toLocaleString()),
        cell(a.duration_ms + " ms"),
        cell(a.status_code ? String(a.status_code) : "–"),
        cell(a.outcome, "outcome " + a.outcome),
        cell(a.error || ""),
      );
      table.append(row);
    }
    td.append(table);
  }
  tr.append(td);
  return tr;
}

async function loadAttempts(id) {
  try {
    const data = await api("GET", "/jobs/" + id + "/attempts");
    if (attempts.has(id)) attempts.set(id, data.attempts);
  } catch (err) {
    if (err instanceof Unauthorized) {
      logout(false);
      return;
    }
    attempts.delete(id);
    $("jobs-error").textContent = err.message;
    $("jobs-error").hidden = false;
  }
  render();
}

function toggleAttempts(id) {
  if (attempts.has(id)) {
    attempts.delete(id);
    render();
    return;
  }
  attempts.set(id, null); // null: loading
  render();
  loadAttempts(id);
}

function render() {
  const list = [...jobs.values()].sort(
    (a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
  const rows = list.flatMap((job) => {
    const tr = document.createElement("tr");
    const status = cell(job.status, "status " + job.status);
    if (job.status === "retrying" && job.next_attempt_at) {
      status.title = "next attempt " + new Date(job.next_attempt_at).toLocaleTimeString();
    }
    const title = cell(job.title, "title");
    if (job.type === "webhook" && job.webhook) {
      const host = document.createElement("span");
      host.className = "host";
      host.textContent = " → " + hostOf(job.webhook.url);
      title.append(host);
    }
    tr.append(
      status,
      title,
      cell(job.attempts + "/" + job.max_attempts),
      cell(new Date(job.created_at).toLocaleString(), "created"),
      cell(job.id.slice(0, 8), "id"),
    );
    const actions = cell("", "actions");
    if (job.status === "dead" || job.status === "failed") {
      actions.append(button("Retry", () => retryJob(job.id)), " ");
    }
    if (job.type === "webhook") {
      actions.append(button(attempts.has(job.id) ? "Hide" : "Attempts", () => toggleAttempts(job.id)), " ");
    }
    actions.append(button("Delete", () => deleteJob(job.id, job.title)));
    tr.append(actions);
    return attempts.has(job.id) ? [tr, attemptsRow(job.id)] : [tr];
  });
  $("jobs").replaceChildren(...rows);
  $("empty").hidden = rows.length > 0;
}

// ---------- actions ----------

function createJob(e) {
  e.preventDefault();
  const input = $("title");
  const title = input.value.trim();
  if (!title) return;
  // One key per click: if the request times out and the user clicks again
  // with the same text, it's a new key and a new job. The key protects
  // against the browser or a proxy repeating this one request.
  const key = crypto.randomUUID();
  handle(async () => {
    const job = await api("POST", "/jobs", { title }, { "Idempotency-Key": key });
    apply(job);
    render();
    input.value = "";
  }, $("jobs-error"));
}

function retryJob(id) {
  handle(async () => {
    apply(await api("POST", "/jobs/" + encodeURIComponent(id) + "/retry"));
    render();
  }, $("jobs-error"));
}

function deleteJob(id, title) {
  if (!confirm('Delete "' + title + '"?')) return;
  handle(async () => {
    await api("DELETE", "/jobs/" + encodeURIComponent(id));
    deleted.add(id);
    jobs.delete(id);
    render();
  }, $("jobs-error"));
}

// ---------- session ----------

async function login(e) {
  e.preventDefault();
  const box = $("login-error");
  box.hidden = true;
  const resp = await fetch(API + "/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email: $("email").value, password: $("password").value }),
  }).catch(() => null);
  $("password").value = "";
  if (!resp || !resp.ok) {
    if (!resp) box.textContent = "Can't reach the server.";
    else if (resp.status === 400 || resp.status === 401) box.textContent = "Wrong email or password.";
    else box.textContent = "Login failed (HTTP " + resp.status + ").";
    box.hidden = false;
    return;
  }
  const data = await resp.json();
  sessionStorage.setItem(TOKEN_KEY, data.token);
  start();
}

// logout ends the session on the server (unless it's already gone) and
// forgets everything the page holds.
async function logout(tellServer = true) {
  if (tellServer && sessionStorage.getItem(TOKEN_KEY)) {
    await api("POST", "/auth/logout").catch(() => {});
  }
  sessionStorage.removeItem(TOKEN_KEY);
  clearTimeout(retryTimer);
  if (events) events.close();
  events = null;
  jobs.clear();
  attempts.clear();
  deleted.clear();
  render();
  $("session").hidden = true;
  $("jobs-view").hidden = true;
  $("login-view").hidden = false;
}

function start() {
  $("login-view").hidden = true;
  $("session").hidden = false;
  $("jobs-view").hidden = false;
  retryDelay = 1000;
  connect();
}

document.addEventListener("DOMContentLoaded", () => {
  $("login-form").addEventListener("submit", login);
  $("create-form").addEventListener("submit", createJob);
  $("logout").addEventListener("click", () => logout(true));
  if (sessionStorage.getItem(TOKEN_KEY)) {
    start();
  } else {
    $("login-view").hidden = false;
  }
});
