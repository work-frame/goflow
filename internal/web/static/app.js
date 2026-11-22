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
//   - The password can be shown while it's typed (setPasswordVisible), but
//     it's hidden again at every login attempt, right or wrong, and at
//     logout, so it's never left on screen. The button's label says what
//     pressing it will do: "Show password" or "Hide password".
//   - A click anywhere on a job's row opens its drawer, except a click that
//     starts on a button (Retry, Delete, Copy ID), which only does what
//     that button says. The title stays a real button, so the keyboard and
//     screen readers have the same way in (Task 79).
//   - The details drawer is drawn from the list's copy of the job, which
//     has no webhook body. It never asks for the single job (that answer
//     carries the body); only the job's attempts, retry and delete.
//   - The overview numbers, search, status filter and pages are all worked
//     out here, from the complete list the page holds, so a live event
//     changes them the moment it arrives. They count your jobs only.
//   - Two health signals, never mixed: "API" is GET /readyz (the API and
//     its database answer). The live dot is the event stream, with three
//     states: live (stream open, events arriving), paused (stream open,
//     but the server says events stopped reaching it, so the list may be
//     stale) and offline/reconnecting (stream closed). An open stream
//     alone never shows "live".
//   - API keys (Task 87): a key's secret is shown once, in the key dialog,
//     and lives only in keySecret and the read-only field while that step
//     is open. It is never put in sessionStorage, localStorage, a URL, the
//     console or an error message, and it reaches the clipboard only when
//     Copy is clicked. "I've saved it" is the only way out of that step:
//     Escape is refused, and if the browser closes the dialog anyway it is
//     opened again. Closing clears the field, the variable and the copy
//     status (clearKeySecret). JavaScript can't wipe a string from memory;
//     dropping every reference to it is as far as a page can go.
"use strict";

const TOKEN_KEY = "goflow.token";
const API = "/api/v1";
const PAGE_SIZE = 25;
const HEALTH_EVERY = 30000;

// Every status the API can return, in the order the filter lists them.
const STATUSES = ["pending", "queued", "processing", "retrying", "completed", "failed", "dead"];

// jobs holds the page's copy of each job by ID. deleted remembers IDs
// deleted while the page is open, so a late "changed" event can't bring
// one back.
const jobs = new Map();
const deleted = new Set();

// view is what the table shows: the filters and the page number.
const view = { text: "", status: "", page: 0 };
let loaded = false;     // the first full load has finished

// drawer is the job the details drawer shows, and its attempts:
// undefined (not a webhook), null (loading), an array, or an Error.
const drawer = { id: "", attempts: undefined };

// keys is the account's API keys as GET /keys gave them, newest first.
let keys = [];
let keysLoaded = false;
// keySecret is a new key's secret, only while the dialog shows it once.
let keySecret = "";
let keySaved = false;   // "I've saved it" was pressed: the dialog may close
let keyOpener = null;   // the control that opened the key dialog
let pendingRevoke = null; // { id } for one key, { all: true } for every key

let events = null;      // the open EventSource, if any
let retryDelay = 1000;  // reconnect backoff, doubling to 30 s
let retryTimer = null;
let healthTimer = null;

const $ = (id) => document.getElementById(id);

// ---------- API calls ----------

class Unauthorized extends Error {}

// api sends one request with the session token and returns the parsed
// JSON (or null for 204). A 401 means the session is gone: log out.
async function api(method, path, body, extraHeaders) {
  const headers = { Authorization: "Bearer " + sessionStorage.getItem(TOKEN_KEY) };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  Object.assign(headers, extraHeaders || {});
  let resp;
  try {
    resp = await fetch(API + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new Error("Can't reach the server.");
  }
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

function showError(box, message) {
  box.textContent = message;
  box.hidden = !message;
}

// handle runs an action and shows its error, or logs out on 401.
async function handle(action, errorBox) {
  showError(errorBox, "");
  try {
    await action();
    return true;
  } catch (err) {
    if (err instanceof Unauthorized) {
      logout(false);
      return false;
    }
    showError(errorBox, err.message);
    return false;
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
  // Open attempts are out of date when their job changes.
  if (drawer.id === job.id && job.type === "webhook") loadAttempts(job.id);
}

function forget(id) {
  deleted.add(id);
  jobs.delete(id);
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
  loaded = true;
  render();
}

// ---------- health ----------

function setLive(state, text) {
  const el = $("live");
  el.className = "live " + state;
  el.textContent = text;
}

function setHealth(state, text) {
  const el = $("api-health");
  el.className = "health " + state;
  el.textContent = text;
}

// checkHealth asks /readyz, which needs no session and says only "ready"
// or "not ready". It runs every 30 s while someone is logged in.
async function checkHealth() {
  clearTimeout(healthTimer);
  try {
    const resp = await fetch("/readyz", { cache: "no-store" });
    if (resp.ok) setHealth("on", "API: ok");
    else setHealth("off", "API: not ready");
  } catch {
    setHealth("off", "API: unreachable");
  }
  if (sessionStorage.getItem(TOKEN_KEY)) healthTimer = setTimeout(checkHealth, HEALTH_EVERY);
}

// ---------- live updates ----------

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
        forget(msg.id);
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

// ---------- small builders ----------

function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

function cell(text, className) {
  return el("td", className, text);
}

function button(label, className, onClick, focusKey) {
  const b = el("button", "btn " + className, label);
  b.type = "button";
  b.addEventListener("click", onClick);
  if (focusKey) b.dataset.focus = focusKey;
  return b;
}

// badge is a job status: a colored dot and the word, so color is never
// the only signal.
function badge(status) {
  const known = STATUSES.includes(status) ? status : "unknown";
  const b = el("span", "badge status-" + known);
  b.append(el("span", "dot"), el("span", "", status));
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

// webhookHost is the one place the page reads a webhook's URL.
function webhookHost(job) {
  if (job.type !== "webhook" || !job.webhook) return "";
  return hostOf(job.webhook.url);
}

function when(iso) {
  return iso ? new Date(iso).toLocaleString() : "–";
}

// short is the table's compact time: "17 Nov, 21:40", with the year only
// when it isn't this year. The full time goes in the cell's title.
function short(iso) {
  if (!iso) return "–";
  const d = new Date(iso);
  const opts = { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
  return d.toLocaleString(undefined, opts);
}

// ago says how far a time is from now, for the drawer: "3 min ago", or
// "in 40 s" for a retry still to come.
function ago(iso) {
  if (!iso) return "";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  const a = Math.abs(s);
  const amount = a < 10 ? "" : a < 60 ? a + " s" : a < 3600 ? Math.round(a / 60) + " min"
    : a < 86400 ? Math.round(a / 3600) + " h" : Math.round(a / 86400) + " days";
  if (!amount) return "just now";
  return s >= 0 ? amount + " ago" : "in " + amount;
}

// timeWithAgo is a drawer value: the full time, then how long ago, muted.
function timeWithAgo(iso) {
  const wrap = el("span", "time");
  wrap.append(el("span", "", when(iso)), el("span", "muted ago", ago(iso)));
  return wrap;
}

function announce(text) {
  $("announce").textContent = text;
}

// ---------- overview ----------

function count(list, ...statuses) {
  return list.filter((j) => statuses.includes(j.status)).length;
}

function renderMetrics(list) {
  const done = count(list, "completed");
  const bad = count(list, "dead", "failed");
  const finished = done + bad;
  const values = {
    "m-total": String(list.length),
    "m-waiting": String(count(list, "pending", "queued")),
    "m-processing": String(count(list, "processing")),
    "m-retrying": String(count(list, "retrying")),
    "m-completed": String(done),
    "m-dead": String(bad),
    // completed ÷ (completed + dead + failed); "—" until something finished.
    "m-rate": finished === 0 ? "—" : (Math.round((done / finished) * 1000) / 10) + "%",
  };
  for (const [id, v] of Object.entries(values)) {
    const dd = $(id);
    dd.textContent = loaded ? v : "0";
    dd.classList.toggle("skeleton-text", !loaded);
  }
  $("metrics").setAttribute("aria-busy", String(!loaded));
}

// ---------- the table ----------

function renderStatusFilter(list) {
  const select = $("filter-status");
  const options = [["", "All statuses (" + list.length + ")"]];
  for (const s of STATUSES) {
    const n = count(list, s);
    if (n > 0 || view.status === s) options.push([s, s[0].toUpperCase() + s.slice(1) + " (" + n + ")"]);
  }
  select.replaceChildren(...options.map(([value, label]) => {
    const o = el("option", "", label);
    o.value = value;
    return o;
  }));
  select.value = view.status;
}

function matches(job) {
  if (view.status && job.status !== view.status) return false;
  if (!view.text) return true;
  const q = view.text.toLowerCase();
  return job.title.toLowerCase().includes(q) || job.id.toLowerCase().startsWith(q);
}

function skeletonRows() {
  return Array.from({ length: 5 }, () => {
    const tr = el("tr", "skeleton-row");
    for (const className of ["status", "title", "num", "col-created", "col-id", "actions"]) {
      const td = cell("", className);
      td.append(el("span", "skeleton"));
      tr.append(td);
    }
    return tr;
  });
}

function jobRow(job) {
  const tr = el("tr");
  const status = cell("", "status");
  status.append(badge(job.status));
  if (job.status === "retrying" && job.next_attempt_at) {
    status.title = "next attempt " + new Date(job.next_attempt_at).toLocaleTimeString();
  }

  const title = cell("", "title");
  const open = button(job.title, "link", () => openDrawer(job.id), "open:" + job.id);
  open.setAttribute("aria-label", "Details for " + job.title);
  title.append(open);
  const host = webhookHost(job);
  if (host) title.append(el("span", "host", "webhook → " + host));

  const actions = cell("", "actions");
  if (job.status === "dead" || job.status === "failed") {
    actions.append(button("Retry", "small action", () => retryJob(job.id, $("jobs-error")), "retry:" + job.id));
  }
  actions.append(button("Delete", "small action delete", () => askDelete(job.id), "delete:" + job.id));

  const created = cell(short(job.created_at), "col-created time");
  created.title = when(job.created_at);
  const idCell = cell("", "col-id");
  idCell.append(copyButton(job.id, job.id.slice(0, 8), "id-copy mono", "copy:" + job.id));

  tr.append(
    status,
    title,
    cell(job.attempts + "/" + job.max_attempts, "num"),
    created,
    idCell,
    actions,
  );
  // The row is a bigger target for a mouse or a finger. A click that
  // starts on a button is that button's: Retry, Delete and Copy ID never
  // open the drawer as well. Selecting text in the row doesn't either.
  tr.className = "row-link";
  tr.addEventListener("click", (e) => {
    if (e.target.closest("button")) return;
    if (String(window.getSelection())) return;
    openDrawer(job.id);
  });
  return tr;
}

// render redraws everything that depends on the job list. A live event
// may arrive while a row button has focus; the button with the same
// data-focus key gets it back, so keyboard users don't lose their place.
function render() {
  const all = [...jobs.values()].sort(
    (a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
  renderMetrics(all);
  renderStatusFilter(all);

  const focusKey = document.activeElement && document.activeElement.dataset
    ? document.activeElement.dataset.focus : undefined;

  const shown = all.filter(matches);
  const pages = Math.max(1, Math.ceil(shown.length / PAGE_SIZE));
  view.page = Math.min(view.page, pages - 1);
  const start = view.page * PAGE_SIZE;
  const slice = shown.slice(start, start + PAGE_SIZE);

  $("jobs").replaceChildren(...(loaded ? slice.map(jobRow) : skeletonRows()));
  $("jobs").closest("table").setAttribute("aria-busy", String(!loaded));

  const filtering = view.text !== "" || view.status !== "";
  $("empty").hidden = !loaded || shown.length > 0;
  if (all.length === 0) {
    $("empty-title").textContent = "No jobs yet";
    $("empty-text").textContent = "Create one above. Try sleep:3s, then watch it move from queued to completed.";
  } else {
    $("empty-title").textContent = "No jobs match";
    $("empty-text").textContent = "Nothing fits this search and status. Clear the filters to see all " + all.length + " jobs.";
  }
  $("empty-clear").hidden = !filtering;

  $("page-info").textContent = !loaded ? "Loading jobs…"
    : shown.length === 0 ? ""
    : "Showing " + (start + 1) + "–" + (start + slice.length) + " of " + shown.length
      + (filtering ? " matching" : "");
  $("page-prev").disabled = view.page === 0;
  $("page-next").disabled = view.page >= pages - 1;

  if (focusKey) {
    const again = document.querySelector('[data-focus="' + CSS.escape(focusKey) + '"]');
    if (again) again.focus();
  }
  renderDrawer();
}

// ---------- the details drawer ----------

// copied remembers the last Copy ID press for 2 s, so the button can say
// how it went even if a live event redraws the table meanwhile.
let copied = { id: "", ok: false, until: 0 };

// copyButton copies a job's full ID. It says "Copied" or "Copy failed" on
// itself, tells screen readers through #announce, and on failure points
// to the drawer, where the full ID can be selected by hand.
function copyButton(id, label, className, focusKey) {
  const fresh = copied.id === id && Date.now() < copied.until;
  const text = !fresh ? label : copied.ok ? "Copied" : "Copy failed";
  const b = button(text, "small " + className, () => copyId(id), focusKey);
  b.setAttribute("aria-label", "Copy job ID " + id);
  b.title = fresh && !copied.ok ? "Your browser didn't allow it: select the ID in the details and copy it by hand" : "Copy the full ID";
  return b;
}

async function copyId(id) {
  let ok = true;
  try {
    await navigator.clipboard.writeText(id);
  } catch {
    ok = false;
  }
  copied = { id, ok, until: Date.now() + 2000 };
  announce(ok ? "Job ID copied" : "Couldn't copy the job ID. Select it in the details and copy it by hand.");
  render();
  setTimeout(render, 2100);
}

function field(dl, label, value, className) {
  const wrap = el("div", "field-row");
  wrap.append(el("dt", "", label));
  const dd = el("dd", className);
  if (value instanceof Node) dd.append(value);
  else dd.textContent = value;
  wrap.append(dd);
  dl.append(wrap);
}

function openDrawer(id) {
  const job = jobs.get(id);
  if (!job) return;
  drawer.id = id;
  drawer.attempts = job.type === "webhook" ? null : undefined;
  showError($("drawer-error"), "");
  renderDrawer();
  const d = $("drawer");
  if (!d.open) d.showModal();
  if (job.type === "webhook") loadAttempts(id);
}

function closeDrawer() {
  drawer.id = "";
  drawer.attempts = undefined;
  if ($("drawer").open) $("drawer").close();
}

function renderAttempts() {
  const box = $("attempts-body");
  const list = drawer.attempts;
  if (list === null) {
    box.replaceChildren(el("p", "muted small", "Loading attempts…"));
    return;
  }
  if (list instanceof Error) {
    box.replaceChildren(el("p", "error small", "Couldn't load attempts: " + list.message));
    return;
  }
  if (list.length === 0) {
    box.replaceChildren(el("p", "muted small", "No attempts yet."));
    return;
  }
  const ol = el("ol", "attempt-list");
  for (const a of list) {
    const li = el("li", "attempt outcome-" + a.outcome);
    const top = el("div", "attempt-top");
    top.append(
      el("span", "attempt-n", "Attempt " + a.attempt),
      el("span", "attempt-outcome", a.outcome),
    );
    const facts = el("dl", "attempt-facts");
    for (const [k, v] of [["HTTP", a.status_code ? String(a.status_code) : "no response"],
      ["Took", a.duration_ms + " ms"], ["Started", when(a.started_at)]]) {
      const d = el("div");
      d.append(el("dt", "", k), el("dd", "", v));
      facts.append(d);
    }
    li.append(top, facts);
    if (a.error) li.append(el("p", "attempt-error mono", a.error));
    ol.append(li);
  }
  box.replaceChildren(ol);
}

function renderDrawer() {
  if (!drawer.id) return;
  const job = jobs.get(drawer.id);
  const gone = !job;
  $("drawer-gone").hidden = !gone;
  $("drawer-delete").disabled = gone;
  for (const id of ["drawer-overview", "drawer-execution", "drawer-attempts"]) $(id).hidden = gone;
  if (gone) {
    $("drawer-retry").hidden = true;
    return;
  }
  $("drawer-title").textContent = job.title;

  // Overview: what the job is.
  const overview = $("drawer-overview-fields");
  overview.replaceChildren();
  const idWrap = el("span", "id-wrap");
  idWrap.append(el("code", "mono", job.id), copyButton(job.id, "Copy", "quiet", "copy-drawer:" + job.id));
  field(overview, "ID", idWrap);
  field(overview, "Type", job.type);
  field(overview, "Created by", createdBy(job));
  field(overview, "Status", badge(job.status));
  const host = webhookHost(job);
  if (host) field(overview, "Delivers to", host, "mono");

  // Execution: how it ran, from the list's copy (no body, no last error:
  // the API doesn't send them to the browser).
  const exec = $("drawer-execution-fields");
  exec.replaceChildren();
  field(exec, "Attempts", job.attempts + " of " + job.max_attempts);
  if (job.status === "retrying" && job.next_attempt_at) field(exec, "Next attempt", timeWithAgo(job.next_attempt_at));
  field(exec, "Created", timeWithAgo(job.created_at));
  field(exec, "Updated", timeWithAgo(job.updated_at));

  $("drawer-retry").hidden = !(job.status === "dead" || job.status === "failed");
  if (job.type === "webhook") {
    renderAttempts();
  } else {
    // Only webhook deliveries are recorded one by one (webhook_attempts).
    // Say so instead of drawing a history that doesn't exist.
    $("attempts-body").replaceChildren(el("p", "muted small",
      "Delivery attempts are recorded for webhook jobs. This job ran " + job.attempts + " of " + job.max_attempts + " times."));
  }
}

// createdBy says who made a job (Task 87, V6): one of your API keys, by
// name and hint, or a logged-in session. A key no longer in the list (it
// can't be, while the account exists, but the page may not have loaded
// it yet) is shown by its short ID.
function createdBy(job) {
  if (!job.api_key_id) return "You, logged in (dashboard or CLI)";
  const k = keys.find((x) => x.id === job.api_key_id);
  if (!k) return "API key " + job.api_key_id.slice(0, 8);
  return "API key: " + k.name + " (gfk_…" + k.hint + ")";
}

async function loadAttempts(id) {
  try {
    const data = await api("GET", "/jobs/" + encodeURIComponent(id) + "/attempts");
    if (drawer.id === id) drawer.attempts = data.attempts;
  } catch (err) {
    if (err instanceof Unauthorized) {
      logout(false);
      return;
    }
    if (drawer.id === id) drawer.attempts = err;
  }
  renderDrawer();
}

// ---------- actions ----------

function createJob(e) {
  e.preventDefault();
  const box = $("create-error");
  const input = $("title");
  const title = input.value.trim();
  const rawMax = $("max-attempts").value.trim();
  if (!title) {
    showError(box, "Enter a title.");
    input.setAttribute("aria-invalid", "true");
    input.focus();
    return;
  }
  input.removeAttribute("aria-invalid");
  const body = { title };
  // Left empty, max_attempts isn't sent and the server keeps its default.
  if (rawMax !== "") {
    const n = Number(rawMax);
    if (!Number.isInteger(n) || n < 1 || n > 10) {
      showError(box, "Max attempts must be a whole number from 1 to 10.");
      $("max-attempts").setAttribute("aria-invalid", "true");
      $("max-attempts").focus();
      return;
    }
    body.max_attempts = n;
  }
  $("max-attempts").removeAttribute("aria-invalid");

  // One key per click: if the request times out and the user clicks again
  // with the same text, it's a new key and a new job. The key protects
  // against the browser or a proxy repeating this one request. The button
  // is disabled until the answer, so one click can't send two requests.
  const key = crypto.randomUUID();
  const submit = $("create-submit");
  submit.disabled = true;
  submit.textContent = "Creating…";
  handle(async () => {
    const job = await api("POST", "/jobs", body, { "Idempotency-Key": key });
    apply(job);
    render();
    input.value = "";
    input.focus();
  }, box).finally(() => {
    submit.disabled = false;
    submit.textContent = "Create job";
  });
}

function retryJob(id, box) {
  return handle(async () => {
    apply(await api("POST", "/jobs/" + encodeURIComponent(id) + "/retry"));
    render();
  }, box);
}

// askDelete opens the confirm dialog; deleteJob runs only on "Delete".
let pendingDelete = "";

function askDelete(id) {
  const job = jobs.get(id);
  if (!job) return;
  pendingDelete = id;
  $("confirm-text").textContent = "“" + job.title + "” and its attempts are removed for good.";
  $("confirm").returnValue = "";
  $("confirm").showModal();
}

function deleteJob(id) {
  const box = drawer.id === id ? $("drawer-error") : $("jobs-error");
  return handle(async () => {
    await api("DELETE", "/jobs/" + encodeURIComponent(id));
    forget(id);
    if (drawer.id === id) closeDrawer();
    render();
  }, box);
}

// ---------- views ----------

// The page has two views, picked by the address's #: #keys shows the API
// keys, anything else the jobs. The # never holds anything secret.
function currentView() {
  return location.hash === "#keys" ? "keys" : "jobs";
}

function showView() {
  const onKeys = currentView() === "keys";
  $("jobs-view").hidden = onKeys;
  $("keys-view").hidden = !onKeys;
  for (const [id, on] of [["nav-jobs", !onKeys], ["nav-keys", onKeys]]) {
    if (on) $(id).setAttribute("aria-current", "page");
    else $(id).removeAttribute("aria-current");
  }
  if (onKeys) loadKeys();
}

// ---------- API keys ----------

function loadKeys() {
  return handle(async () => {
    keys = (await api("GET", "/keys")).keys;
    keysLoaded = true;
    renderKeys();
    renderDrawer();
  }, $("keys-error"));
}

const KEY_LIMIT = 10;

function keyBadge(status) {
  const known = ["active", "expired", "revoked"].includes(status) ? status : "expired";
  const b = el("span", "badge key-" + known);
  b.append(el("span", "dot"), el("span", "", status));
  return b;
}

// labelled is a table cell that, on a phone, shows its column's name
// before its value (the rows become cards there; see style.css).
function labelled(label, content, className) {
  const td = el("td", className);
  td.dataset.label = label;
  if (content instanceof Node) td.append(content);
  else td.textContent = content;
  return td;
}

function keyRow(k) {
  // "is-", not "key-": key-active and the rest are the badge's colors.
  const tr = el("tr", "is-" + k.status);
  const scopes = el("span", "scope-list");
  for (const s of k.scopes) scopes.append(el("code", "", s));
  const created = labelled("Created", short(k.created_at), "time");
  created.title = when(k.created_at);
  const used = labelled("Last used", k.last_used_at ? ago(k.last_used_at) : "never", "time");
  if (k.last_used_at) used.title = when(k.last_used_at);
  const expires = labelled("Expires", short(k.expires_at), "time");
  expires.title = when(k.expires_at);
  const actions = labelled("", "", "actions");
  if (k.status === "active") {
    const b = button("Revoke", "small action delete", () => askRevoke(k), "revoke:" + k.id);
    b.setAttribute("aria-label", "Revoke key " + k.name);
    actions.append(b);
  }
  tr.append(
    labelled("Name", k.name, "key-name"),
    labelled("Status", keyBadge(k.status)),
    labelled("Scopes", scopes),
    labelled("Key", "gfk_…" + k.hint, "mono"),
    created, used, expires, actions,
  );
  return tr;
}

function renderKeys() {
  const active = keys.filter((k) => k.status === "active").length;
  $("keys").replaceChildren(...keys.map(keyRow));
  $("keys-empty").hidden = !keysLoaded || keys.length > 0;
  $("keys-count").textContent = keysLoaded ? active + " of " + KEY_LIMIT + " active" : "Loading keys…";
  $("key-revoke-all").disabled = active === 0;
  $("key-new").disabled = keysLoaded && active >= KEY_LIMIT;
  $("key-new").title = $("key-new").disabled ? "You have " + KEY_LIMIT + " active keys: revoke one first" : "";
}

function setKeyPasswordVisible(visible) {
  $("key-password").type = visible ? "text" : "password";
  const b = $("key-password-toggle");
  b.textContent = visible ? "Hide" : "Show";
  b.setAttribute("aria-label", visible ? "Hide password" : "Show password");
}

// clearKeySecret forgets a shown secret: the field, the variable and the
// copy status, and puts the dialog back on its form.
function clearKeySecret() {
  keySecret = "";
  $("key-secret").value = "";
  $("key-copy-status").textContent = "";
  $("key-reveal").hidden = true;
  $("key-form").hidden = false;
}

function openKeyDialog() {
  keyOpener = document.activeElement;
  keySaved = false;
  clearKeySecret();
  $("key-name").value = "";
  $("key-scope-create").checked = true;
  $("key-scope-read").checked = false;
  $("key-expiry").value = "90";
  $("key-password").value = "";
  setKeyPasswordVisible(false);
  showError($("key-error"), "");
  $("key-dialog").showModal();
  $("key-name").focus();
}

// closeKeyDialog is the only way the dialog closes on purpose: Cancel on
// the form, or "I've saved it" once the secret has been shown.
function closeKeyDialog() {
  keySaved = true;
  clearKeySecret();
  $("key-password").value = "";
  setKeyPasswordVisible(false);
  if ($("key-dialog").open) $("key-dialog").close();
  if (keyOpener && document.contains(keyOpener)) keyOpener.focus();
  keyOpener = null;
}

function sentence(msg) {
  return msg ? msg[0].toUpperCase() + msg.slice(1) + (msg.endsWith(".") ? "" : ".") : "";
}

async function createKey(e) {
  e.preventDefault();
  setKeyPasswordVisible(false);
  const box = $("key-error");
  showError(box, "");
  const name = $("key-name").value.trim();
  const scopes = [$("key-scope-create"), $("key-scope-read")].filter((c) => c.checked).map((c) => c.value);
  const password = $("key-password").value;
  if (!name) return showError(box, "Enter a name, such as the program that will use the key.");
  if (scopes.length === 0) return showError(box, "Pick at least one scope.");
  if (!password) return showError(box, "Enter your password.");

  const submit = $("key-create");
  submit.disabled = true;
  submit.textContent = "Creating…";
  let data;
  try {
    data = await api("POST", "/keys", { name, scopes, expires_in_days: Number($("key-expiry").value), password });
  } catch (err) {
    if (err instanceof Unauthorized) {
      closeKeyDialog();
      logout(false);
      return;
    }
    showError(box, err.status === 403 ? "Wrong password." : sentence(err.message));
    return;
  } finally {
    $("key-password").value = "";
    submit.disabled = false;
    submit.textContent = "Create key";
  }
  // Only a real key from a successful answer is ever shown (condition 3).
  if (!data || typeof data.secret !== "string" || !/^gfk_[A-Za-z0-9_-]{43}$/.test(data.secret)) {
    showError(box, "The server's answer had no key. If a new key shows up in the list, revoke it.");
    loadKeys();
    return;
  }
  keySecret = data.secret;
  data = null;
  $("key-secret").value = keySecret;
  $("key-form").hidden = true;
  $("key-reveal").hidden = false;
  $("key-secret").focus();
  $("key-secret").select();
  announce("Key created. This is the only time it is shown.");
  loadKeys();
}

// copyKey copies the secret only because Copy was clicked (condition 1).
async function copyKey() {
  let ok = true;
  try {
    await navigator.clipboard.writeText(keySecret);
  } catch {
    ok = false;
  }
  $("key-copy-status").textContent = ok
    ? "Copied. Paste it where your program keeps its secrets."
    : "Copy failed: select the key above and copy it by hand.";
}

function askRevoke(k) {
  pendingRevoke = { id: k.id };
  $("key-confirm-title").textContent = "Revoke this key?";
  $("key-confirm-text").textContent = "“" + k.name + "” (gfk_…" + k.hint + ") stops working at once: programs using it get 401 from their next request. It stays in the list as revoked.";
  $("key-confirm-ok").textContent = "Revoke";
  $("key-confirm").returnValue = "";
  $("key-confirm").showModal();
}

function askRevokeAll() {
  const n = keys.filter((k) => k.status === "active").length;
  if (n === 0) return;
  pendingRevoke = { all: true };
  $("key-confirm-title").textContent = "Revoke all keys?";
  $("key-confirm-text").textContent = "All " + n + " active " + (n === 1 ? "key stops" : "keys stop") + " working at once: programs using them get 401 from their next request.";
  $("key-confirm-ok").textContent = "Revoke all";
  $("key-confirm").returnValue = "";
  $("key-confirm").showModal();
}

function revoke(what) {
  return handle(async () => {
    if (what.all) {
      const r = await api("POST", "/keys/revoke-all");
      announce(r.revoked + " keys revoked");
    } else {
      await api("DELETE", "/keys/" + encodeURIComponent(what.id));
      announce("Key revoked");
    }
    await loadKeys();
    $("keys-heading").focus();
  }, $("keys-error"));
}

// ---------- session ----------

// setPasswordVisible shows the password as text or hides it again. The
// button's visible text and its screen-reader label always say what a
// press will do next.
function setPasswordVisible(visible) {
  $("password").type = visible ? "text" : "password";
  const b = $("password-toggle");
  b.textContent = visible ? "Hide" : "Show";
  b.setAttribute("aria-label", visible ? "Hide password" : "Show password");
}

async function login(e) {
  e.preventDefault();
  setPasswordVisible(false); // every attempt hides it, whatever happens next
  const box = $("login-error");
  showError(box, "");
  const email = $("email").value.trim();
  if (!email || !$("password").value) {
    showError(box, "Enter your email and password.");
    return;
  }
  const submit = $("login-submit");
  submit.disabled = true;
  submit.textContent = "Logging in…";
  const resp = await fetch(API + "/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password: $("password").value }),
  }).catch(() => null);
  $("password").value = "";
  submit.disabled = false;
  submit.textContent = "Log in";
  if (!resp || !resp.ok) {
    if (!resp) showError(box, "Can't reach the server.");
    else if (resp.status === 400 || resp.status === 401) showError(box, "Wrong email or password.");
    else showError(box, "Login failed (HTTP " + resp.status + ").");
    $("password").focus();
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
  setPasswordVisible(false);
  clearTimeout(retryTimer);
  clearTimeout(healthTimer);
  if (events) events.close();
  events = null;
  closeDrawer();
  if ($("confirm").open) $("confirm").close();
  if ($("key-confirm").open) $("key-confirm").close();
  closeKeyDialog();
  keys = [];
  keysLoaded = false;
  renderKeys();
  jobs.clear();
  deleted.clear();
  loaded = false;
  view.text = "";
  view.status = "";
  view.page = 0;
  $("filter-text").value = "";
  $("who").textContent = "";
  render();
  $("session").hidden = true;
  $("views").hidden = true;
  $("jobs-view").hidden = true;
  $("keys-view").hidden = true;
  $("login-view").hidden = false;
  $("email").focus();
}

async function loadMe() {
  try {
    const me = await api("GET", "/auth/me");
    $("who").textContent = me.email;
  } catch (err) {
    if (err instanceof Unauthorized) logout(false);
  }
}

function start() {
  $("login-view").hidden = true;
  $("session").hidden = false;
  $("views").hidden = false;
  showView();
  retryDelay = 1000;
  render();
  // The drawer names the key that made a job, so the list is needed on
  // the jobs view too.
  if (currentView() !== "keys") loadKeys();
  loadMe();
  checkHealth();
  connect();
}

document.addEventListener("DOMContentLoaded", () => {
  $("login-form").addEventListener("submit", login);
  $("create-form").addEventListener("submit", createJob);
  $("logout").addEventListener("click", () => logout(true));
  $("password-toggle").addEventListener("click", () => {
    setPasswordVisible($("password").type === "password");
    $("password").focus();
  });

  $("filter-text").addEventListener("input", (e) => {
    view.text = e.target.value.trim();
    view.page = 0;
    render();
  });
  $("filter-status").addEventListener("change", (e) => {
    view.status = e.target.value;
    view.page = 0;
    render();
  });
  $("empty-clear").addEventListener("click", () => {
    view.text = "";
    view.status = "";
    view.page = 0;
    $("filter-text").value = "";
    render();
    $("filter-text").focus();
  });
  $("page-prev").addEventListener("click", () => { view.page--; render(); });
  $("page-next").addEventListener("click", () => { view.page++; render(); });

  $("drawer-close").addEventListener("click", closeDrawer);
  $("drawer-close-bottom").addEventListener("click", closeDrawer);
  $("drawer").addEventListener("close", () => { drawer.id = ""; drawer.attempts = undefined; });
  // A click on the backdrop (the dialog itself, outside its content) closes it.
  $("drawer").addEventListener("click", (e) => { if (e.target === $("drawer")) closeDrawer(); });
  $("drawer-retry").addEventListener("click", () => retryJob(drawer.id, $("drawer-error")));
  $("drawer-delete").addEventListener("click", () => askDelete(drawer.id));

  window.addEventListener("hashchange", () => {
    if (sessionStorage.getItem(TOKEN_KEY)) showView();
  });
  $("key-new").addEventListener("click", openKeyDialog);
  $("key-revoke-all").addEventListener("click", askRevokeAll);
  $("key-form").addEventListener("submit", createKey);
  $("key-cancel").addEventListener("click", closeKeyDialog);
  $("key-copy").addEventListener("click", copyKey);
  $("key-saved").addEventListener("click", closeKeyDialog);
  $("key-password-toggle").addEventListener("click", () => {
    setKeyPasswordVisible($("key-password").type === "password");
    $("key-password").focus();
  });
  // While the secret is shown, Escape does nothing (V4). Browsers may
  // still close a dialog on a second Escape; then it opens again, secret
  // and all, until "I've saved it" is pressed.
  $("key-dialog").addEventListener("cancel", (e) => {
    if (keySecret) e.preventDefault();
    else keySaved = true;
  });
  $("key-dialog").addEventListener("close", () => {
    if (keySecret && !keySaved) {
      $("key-dialog").showModal();
      $("key-secret").focus();
      return;
    }
    clearKeySecret();
    $("key-password").value = "";
  });
  $("key-confirm").addEventListener("close", () => {
    const what = pendingRevoke;
    pendingRevoke = null;
    if ($("key-confirm").returnValue === "revoke" && what) revoke(what);
  });

  $("confirm").addEventListener("close", () => {
    const id = pendingDelete;
    pendingDelete = "";
    if ($("confirm").returnValue === "delete" && id) deleteJob(id);
  });

  if (sessionStorage.getItem(TOKEN_KEY)) {
    start();
  } else {
    $("login-view").hidden = false;
  }
});
