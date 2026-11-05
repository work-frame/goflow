# Deployment

GoFlow's API runs on [Render](https://render.com) (free web service) with its database on [Neon](https://neon.com) (free PostgreSQL). This file records how it is set up, what was measured, and what the free plans can't do.

Free hosting is for a first deployment and for testing. It is not yet a service people should depend on (see [Limits](#limits-of-the-free-setup)).

## Layout

```text
goflow CLI ──HTTPS──► Render edge ──► goflow-api (Render, Frankfurt) ──TLS verify-full──► Neon PostgreSQL 17 (AWS Frankfurt)
```

| Part | Where | Settings |
|---|---|---|
| API | Render web service `goflow-api`, free plan, region `frankfurt` | 0.1 CPU, 512 MB. Built and started from [`render.yaml`](render.yaml) |
| Database | Neon project `goflow`, branch `production`, AWS Europe Central 1 (Frankfurt) | Postgres 17, free plan, no IP restrictions |
| Queue | Render Key Value `goflow-queue`, free plan, `frankfurt` | Valkey 8. `ipAllowList: []`: reachable only over Render's private network, at `redis://red-...:6379`. `maxmemoryPolicy: noeviction`. No persistence |
| Workers | Inside `goflow-api` (`GOFLOW_WORKERS=2`) | Render's free plan has no background workers ($7/month). The workers sleep when the API sleeps; jobs wait in Redis meanwhile |
| Public URL | `https://goflow-api-hewh.onrender.com` | Plain `http://` answers `301` to `https://` (Render does this) |

The API and the database sit in the same region, so a query between them takes milliseconds. Your distance to Frankfurt is what you notice.

## Settings

| Variable | Value on Render | Why |
|---|---|---|
| `GOFLOW_DATABASE_URL` | Neon's **direct** connection string, ending in `?sslmode=verify-full` | Secret: typed into the Render dashboard, never committed (`sync: false`) |
| `GOFLOW_REGISTRATION` | `closed` | Strangers can't create accounts. Typed into the dashboard |
| `GOFLOW_BCRYPT_COST` | `10` | Set in `render.yaml`. The lowest the server accepts; cost 12 would take seconds per login on 0.1 CPU |
| `PORT` | set by Render | The API listens on it because `GOFLOW_ADDR` is unset |

### The database URL

Neon's **Connect** dialog gives a string like this (password shown as `<password>`):

```text
postgresql://neondb_owner:<password>@ep-....c-8.eu-central-1.aws.neon.tech/neondb?sslmode=require&channel_binding=require
```

Change two things before using it:

1. **Turn connection pooling off** in the dialog, so the host has no `-pooler`. GoFlow keeps its own pool, runs migrations under an advisory lock, and uses prepared statements. Neon's docs say to use a direct connection for those.
2. **Replace everything after `?` with `sslmode=verify-full`.** `verify-full` checks the certificate and the host name, and it makes Go send the host name (SNI) that Neon needs to route the connection. `channel_binding` isn't needed with it.

The API refuses to start if a remote database URL doesn't ask for TLS ([FAILURES.md](FAILURES.md), case 24).

Keep the URL in a password manager. Never paste it into a chat, a terminal command, a `.env` file in the repo, or a screenshot. To use it in a shell (zsh), read it into a variable that isn't saved to history. Paste this line on its own:

```bash
read -s "NEON?Neon URL: " && echo
```

Then check the ending, and fix it if the URL came straight from Neon:

```bash
NEON="${NEON%%\?*}?sslmode=verify-full"   # drops everything after ? and adds verify-full
print -r -- "end: ${(q+)NEON[-30,-1]}"    # shows only the end, never the password
```

## First deployment

1. **Neon:** create the project (Postgres 17, AWS Frankfurt). Copy the direct connection string and fix its ending as above.
2. **Create the first account from your own machine,** so the public server never has registration open:
   ```bash
   # terminal 1, after `read -s NEON` above
   GOFLOW_DATABASE_URL="$NEON" GOFLOW_ADDR=127.0.0.1:9191 GOFLOW_REGISTRATION=open GOFLOW_BCRYPT_COST=10 go run ./cmd/api
   ```
   This applies the migrations to Neon. In a second terminal, register with `read EMAIL`, `read -s PW` and the `jq env.P` pattern from the [README](README.md#examples), against `http://127.0.0.1:9191`. Then **close terminal 1's window** and check `ss -ltn | grep 9191` prints nothing.
3. **Render:** New → Blueprint → the `goflow` repository. Give Render's GitHub app access to only the repositories it deploys. Fill in `GOFLOW_DATABASE_URL` and `GOFLOW_REGISTRATION=closed`. Don't screenshot that form: it shows the URL in plain text.
4. **Check the logs** for `starting ... registration_open=false`, `database connected`, `migrations applied`, `listening`, then the status **Live**.

After that, every merge to `main` deploys automatically (`autoDeployTrigger: commit`).

## Checks after a deploy

```bash
export GOFLOW_API_URL=https://goflow-api-hewh.onrender.com
for p in healthz readyz; do curl -sS -o /dev/null -w "$p %{http_code} %{time_total}s\n" "$GOFLOW_API_URL/$p"; done
curl -sS -w '\nHTTP %{http_code}\n' -X POST -H 'Content-Type: application/json' -d '{}' "$GOFLOW_API_URL/api/v1/auth/register"   # expect 403
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' "http://${GOFLOW_API_URL#https://}/healthz"                      # expect 301 to https
goflow login && goflow job create deploy check && goflow job list
```

## Measured (2 November 2026)

Measured from Nigeria. The client side went over a phone's mobile hotspot.

| What | Result |
|---|---|
| Render build (`go build -trimpath`) | 51.8 s |
| Migrations 1–5 from a laptop in Nigeria to Neon | 72 s: several round trips per migration, plus Neon waking up |
| `healthz` / `readyz`, warm | 0.6–0.85 s each. TCP connect 0.18–0.27 s, TLS 0.2–0.3 s, server answer 0.23 s |
| `readyz` straight after the deploy | 2.1 s (Neon waking up) |
| `register` on a closed server | `403` |
| `goflow job create` / `job list`, warm | 0.75 s / 0.8 s |
| First request after a few idle minutes | 2.4–5.1 s, then back to 0.6 s. `healthz`, which never touches the database, was also slow (2.85 s) |
| Render instance sleeping | Not seen yet: the instance ran from 16:14 to at least 21:37 without a restart |

Almost all of the 0.6–0.8 s is distance: each CLI command opens a new TLS connection to Frankfurt. The server's own work takes milliseconds.

**The slow first request** is most likely the phone's mobile-data radio waking up after idling (the hotspot also dropped DNS queries earlier that day), plus Neon resuming if it had been idle for 5 minutes. This is not yet confirmed with the server-side `duration=` in Render's logs. A cold start of the Render instance itself has not been measured yet.

## Limits of the free setup

| Limit | Effect | Plan |
|---|---|---|
| Render free instances spin down after 15 minutes without requests | The next request waits for a new instance; Render says 50 s or more | The CLI waits up to 90 s. A paid instance or another host before real users |
| Neon free compute suspends after 5 minutes idle | The next query waits for it to resume, and the pool's idle connections are gone (`database/sql` also closes them after 5 minutes idle) | Acceptable for testing |
| 0.1 CPU | bcrypt cost 10 instead of 12 | Raise `GOFLOW_BCRYPT_COST` on a bigger instance; users are re-hashed at their next login |
| One instance; no backups beyond Neon's 6-hour history | A bad deploy or a deleted row has no easy undo | Phase 14 (CI/CD) and a backup plan before real users |
| Rate limits count in memory while Key Value is down | A restart (every 15 idle minutes) resets them until Redis is back | Acceptable for a free setup; a paid Key Value with persistence keeps counters |
| Free Key Value has no persistence | A restart empties both lists; queued jobs lose their IDs | The reconciler re-sends jobs `queued` for over 10 minutes, at start and hourly |
| Neon free: 100 compute-hours a month; can't stay awake for free | A query every few seconds would use the month up in about 2.5 weeks, then suspend the database | The relay never polls while healthy; the reconciler runs hourly. Watch Neon's usage page |
| Free services can't set a shutdown delay | Render rejects `maxShutdownDelaySeconds` on the free plan, so the time between `SIGTERM` and `SIGKILL` is Render's to choose. The pool drains for 20 s; a job cut off sooner records nothing | The next start resets it and runs it again. Set the delay on a paid instance |
| Workers inside the API | One API instance only (startup recovery resets `processing` jobs); jobs run on the API's tenth of a CPU | A paid background worker and leases (Phase 12) before more instances |
| Metrics aren't collected, only read by hand | No history or alerts; counters restart with the instance | A collector (Grafana Cloud's free plan, or Prometheus in Docker) when the instance is paid and always on anyway |

## Queue and workers (Task 27)

Merging a change to `render.yaml` updates the Blueprint: Render creates `goflow-queue` and sets `GOFLOW_REDIS_URL` and `GOFLOW_WORKERS` on the API by itself. Check after the deploy:

1. **Dashboard:** `goflow-queue` exists, plan **Free**, region **Frankfurt**, status **Available**.
2. **API logs:** `queue_enabled=true workers=2`, `queue connected`, `worker pool started workers=2`, and `relay: jobs queued` for jobs that were waiting in the outbox.
3. **End to end:** `goflow job create hosted queue test`, then after about 5 s `goflow job list` shows it `completed`.

What happened on 3 November 2026:

- The first sync failed: `services[0].maxShutdownDelaySeconds: max shutdown delay is not supported for free tier services`. The API's code still deployed (code deploys don't wait for the Blueprint) and ran with the queue off.
- After that was removed (`42fde11`), the sync created `goflow-queue` and redeployed the API with the new settings, and that deploy **failed**: the new instance logged `starting ... queue_enabled=true workers=2` twice, then exited. The old instance kept serving, so nothing was down. Render hides the logs of a failed deploy, so the error was never read; most likely the API started before the new Key Value accepted connections, and the API then exited when its first Redis ping failed.
- A manual **Deploy latest commit** a few minutes later went live. Both waiting jobs (one created on 2 November, one minutes earlier) completed within 30 s of the start, on two workers at once.
- Since Task 28, the API starts even when Redis is unreachable, so a sync that creates a Key Value and redeploys the API at the same time can't fail this way.

## Retries live (Task 30)

```bash
export GOFLOW_API_URL=https://goflow-api-hewh.onrender.com
goflow job create flaky:2 && goflow job create fail && goflow job create fail:permanent && goflow job create hang
goflow job list     # run again every 10-20 s and watch STATUS and TRIES
```

Expected within a few minutes: `flaky:2` → `retrying` 1/3, `retrying` 2/3, `completed` 3/3; `fail` → `dead` 3/3; `fail:permanent` → `dead` 1/3 at once; `hang` → `retrying` after 15 s each time, then `dead` 3/3. In the logs: `worker: job failed, will retry`, `promoter: retries due, queued again`, `worker: job dead`. A `promoter: can't reach Redis` with a script error would mean Key Value refuses Lua; overdue retries would then come back through the reconciler instead.

Result on 3 November 2026 (checked at 23:12 WAT):

| Job | Status | TRIES |
|---|---|---|
| `flaky:2` | `completed` | 3/3 |
| `fail` | `dead` | 3/3 |
| `fail:permanent` | `dead` | 1/3 |
| `hang` | `dead` | 3/3 |

Every job ended where it should. Two things this proved on the hosted setup: Render Key Value runs the promoter's Lua script (`flaky:2` and `fail` came back from the delayed set twice each), and the 15-second deadline stops a hung job on 0.1 CPU as it does locally (FAILURES.md, case 33).

## Re-runs and idempotent create live (Task 31)

After the deploy that applies migration 8 (`migrations applied versions=[8]` in the logs):

```bash
goflow job list --status dead                # the dead jobs from Task 30
goflow job retry <id of fail:permanent>      # prints: <id> is pending again (attempts reset to 0 of 3)
goflow job list --status dead                # it's back, dead 1/3: a worker ran it again
goflow job retry <id of a completed job>     # refused: only a dead or failed job can be retried (HTTP 409)
```

The idempotency key needs `curl` with the `gf` function from the [README](README.md#examples): send the same create twice with one key, and the second answer is `200` with the same ID.

Result on 3 November 2026, about 23:40 WAT (deploy `2ecafb2` built in 34.9 s; the CLI built locally with `go build -o bin/goflow ./cmd/goflow`):

| Check | Result |
|---|---|
| `goflow job retry` on `fail:permanent` (dead 1/3) | `pending again (attempts reset to 0 of 3)`; 15 s later `dead 1/3` again, `updated_at` 22:41:31 UTC against `created_at` 22:12:55: a worker ran it a second time |
| `goflow job retry` on the completed `flaky:2` | `only a dead or failed job can be retried (HTTP 409)` |
| Same create twice with one `Idempotency-Key` | One ID both times: `201`, then `200`. One job in the list |
| Create to claim | The replayed job was already `processing` 18 ms after its `created_at`: outbox, relay, Key Value and a worker's claim, with no polling delay |

The replay showed `processing, 1 attempt` while the first answer had said `pending, 0 attempts`: GoFlow replays the job's current state, not the first response ([ARCHITECTURE.md](ARCHITECTURE.md#idempotent-creation)).

## Live updates live (Task 33)

On 4 November 2026 (deploy `d661403`), with a ticket from `POST /api/v1/events/ticket` and `curl -N` on the stream, `goflow job create flaky:1` from a second terminal produced:

| Time (UTC) | Event | What it shows |
|---|---|---|
| 23:28:31.331 | `pending` | Announced by the API after the insert committed |
| 31.363 | `processing`, attempt 1 | Claimed 32 ms later. No `queued`: the worker claimed it before the relay marked it |
| 31.366 | `retrying`, next attempt 32.714 | A backoff of 1.35 s, inside the 1–2 s range for attempt 1 |
| 33.501 | `processing`, attempt 2 | 0.79 s after it was due: the promoter checks every second |
| 34.512 | `completed` | `flaky` works for 1 s when it succeeds |

From create to `completed`: 3.2 s, every step pushed to the terminal without polling. The used ticket then got `401 {"error":"invalid or expired ticket"}`. Tickets and streams need Redis; nothing else changed in the Render setup.

## Job versions and the dashboard (Tasks 34–35)

After the deploys that bring migration 9 and the dashboard:

1. **Logs:** `migrations applied versions=[9]` once, after the Task 34 deploy.
2. **Browser:** open https://goflow-api-hewh.onrender.com/ and log in. The dot next to **Log out** should say `live`. If the instance was asleep, the first load waits for it to wake (up to a minute).
3. **Live:** create `flaky:1` and `fail:permanent`. Without a refresh, `flaky:1` should go to `retrying` and then `completed`, and `fail:permanent` to `dead`. Click **Retry** on the dead one: it goes back to `pending` and ends `dead` again.
4. **Headers:** `curl -sI https://goflow-api-hewh.onrender.com/` shows the `Content-Security-Policy` line.

Result on 4 November 2026, about 00:52 WAT (deploy `d1b69ad`, built in 33.7 s):

| Check | Result |
|---|---|
| Open https://goflow-api-hewh.onrender.com/ and log in | The page loaded all 10 jobs from Neon, newest first; the dot said `live`; the theme followed the system's dark mode |
| `flaky:1` created in the page | Went through its statuses on screen without a refresh and ended `completed 2/3` |
| `fail:permanent.` (typed with a trailing full stop) | Ran as a plain `fail`, since test titles must match exactly: retried, then `dead 3/3`. Correct for the title it was given |
| `curl -sI` on `/` | `content-security-policy: default-src 'none'; script-src 'self'; ...` and `x-content-type-options: nosniff` |
| Migration 9 | Every job query reads `version`, so the list loading at all shows it applied |

## Finding the real client IP (Task 37)

Rate limiting by IP (Phase 9) needs the client's real address. On Render a request passes through Cloudflare and Render's proxy, so the connection's own address (`remote_addr`) is a proxy's, and the client's address is in a header. Which header can be trusted isn't documented by Render. Community sources say `X-Forwarded-For` is appended to, not replaced, so a client can put a fake address at its start, and that Cloudflare's `True-Client-IP`/`CF-Connecting-IP` are set by the proxy. Those sources are old, so this checks it live. A header the client can fake is worse than none: an attacker would send a new fake address with every request and never hit a per-IP limit.

1. **Turn the diagnostic on.** Render dashboard → `goflow-api` → **Environment** → add `GOFLOW_LOG_IP_HEADERS` = `on` → save and deploy. The start of the log then says `GOFLOW_LOG_IP_HEADERS is on`.
2. **Send two requests** from your own machine, one plain and one with made-up values in every header (these addresses are reserved for documentation and belong to nobody):
   ```bash
   curl -s -o /dev/null https://goflow-api-hewh.onrender.com/readyz
   curl -s -o /dev/null -H 'X-Forwarded-For: 192.0.2.1' -H 'True-Client-IP: 192.0.2.2' -H 'CF-Connecting-IP: 192.0.2.3' -H 'X-Real-IP: 192.0.2.4' https://goflow-api-hewh.onrender.com/readyz
   ```
3. **Read the two `client ip sources` lines with `path=/readyz`** in Render's logs (Render's own health checks add lines for `/healthz`; skip those). For each header, compare the two lines:
   - your real address in both lines: the proxy sets that header itself and ignores yours. **Trustworthy.**
   - `192.0.2.x` in the second line: the client chose it. **Never trust it.**
   - your address after the fake one (`192.0.2.1, <you>`): appended to. Only the entries the proxies added, counted from the right, could be used.
4. **Turn it off:** remove `GOFLOW_LOG_IP_HEADERS` (or set it to `off`) and deploy again. Every request's forwarding headers in the log are noise, and addresses are personal data.

**Result on 4 November 2026 (22:53 UTC, deploy `fddf341`).** Sending all four made-up headers at once got `403` from Cloudflare (`server: cloudflare`, `error code: 1000`): GoFlow never saw the request. One header at a time:

| Header sent | Answer | What reached GoFlow | Verdict |
|---|---|---|---|
| `CF-Connecting-IP: 192.0.2.3` | `403` from Cloudflare | Nothing | A client can't send it. **Used** (`GOFLOW_CLIENT_IP_HEADER=CF-Connecting-IP` in `render.yaml`) |
| `True-Client-IP: 192.0.2.2` | `200` | The real address: replaced | Trustworthy too |
| `X-Forwarded-For: 192.0.2.1` | `200` | `192.0.2.1,<real>, 172.71.95.34, 10.195.74.76`: appended to | The client's fake stays first. The real address is third from the right, which breaks if a hop is added. Never used |
| `X-Real-IP: 192.0.2.4` | `200` | Nothing: removed | Useless |

The connection's own address was `[::1]:38350` for every request through Cloudflare: a local proxy on the instance, the same for everyone. Render's health checks reach `/healthz` directly from `10.231.26.255` with no forwarding headers. The diagnostic was turned off afterwards.

Your public IP address appears in those lines. It isn't a secret like a password, but if you share the lines, you can replace it with `MYIP` everywhere: the comparison only needs to know where it appears.

## Rate limits (Task 38)

After the deploy, check from your machine (paste each block on its own):

```bash
for i in $(seq 1 11); do curl -s -o /dev/null -w "%{http_code} " -X POST -H 'Content-Type: application/json' -d '{"email":"nobody@example.com","password":"not the password"}' https://goflow-api-hewh.onrender.com/api/v1/auth/login; done; echo
```

Expect ten `401` and then `429`. Then:

```bash
curl -s -D - -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"email":"nobody@example.com","password":"x"}' https://goflow-api-hewh.onrender.com/api/v1/auth/login | grep -i -E '^HTTP|retry-after'
```

Expect `429` and a `retry-after` of up to 900 seconds. These attempts also count against your own address (30 per 15 minutes), so wait or log in from the dashboard afterwards to see that your real account still works: its email has its own counter.

Result on 5 November 2026, about 14:13 UTC (deploy `a5314fc`):

| Check | Result |
|---|---|
| 11 wrong logins for `nobody@example.com` | `401` ten times, then `429` |
| A 12th | `HTTP/2 429`, `retry-after: 871`: the window started with the first attempt, about 29 s earlier, and refused attempts don't move it |
| `rate limited` in the logs | `rule=login_email retry_after_s=873` and `871`, no email, no address |
| `client_ip_header` in the `starting` lines | `""` at 14:08:32, then `CF-Connecting-IP` at 14:09:15 and after a restart at 14:31:24 |

**The 43-second gap.** Merging changed both the code and `render.yaml`, and Render deployed them separately: first the code (Auto-Deploy), then the Blueprint sync with the new setting. For 43 seconds the new code ran without `GOFLOW_CLIENT_IP_HEADER`, so every user's login counted against one address, Render's local proxy `[::1]`. The per-email limit worked throughout, so no guess got through; the risk was only that 30 logins from anyone could have blocked everyone's logins until that window ended. Lesson for settings that change behaviour: add the setting in one deploy and the code that needs it in the next, or make the code safe without it.

## Metrics (Task 40)

The Blueprint sync creates `GOFLOW_METRICS_TOKEN` with a random value. To look at the metrics (paste each block on its own):

1. Copy the token from Render: `goflow-api` → **Environment** → `GOFLOW_METRICS_TOKEN` → reveal and copy. Don't paste it into a chat or a screenshot.
2. Read it into your shell without echo:
   ```bash
   read -s "MT?Metrics token: " && echo && echo ${#MT}
   ```
   It should print a length of 32 or more (Render generates 44).
3. Fetch GoFlow's own metrics, the token sent on stdin so it isn't in the process list:
   ```bash
   printf 'Authorization: Bearer %s\n' "$MT" | curl -s -H @- https://goflow-api-hewh.onrender.com/metrics | grep '^goflow_'
   ```
4. `unset MT` when done.

After Task 41 the same command also shows jobs (`goflow_job_status_changes_total`, `goflow_job_attempt_duration_seconds`), the queue (`goflow_queue_jobs{list="pending"}` and the others), live events and rate limits. Create a `flaky:1` job in the dashboard first, and its `retrying` and `completed` changes and both attempts appear.

Result on 5 November 2026 (deploy `1b77b10`, Task 40): without the token, `{"error":"a metrics token is required"}` and `401`; with it, `goflow_http_requests_total` for `GET /healthz` (31 checks in about 2.5 minutes, 1.1 ms in total), `GET /{$}` (the dashboard) and the refused `GET /metrics`, and no path, ID or email anywhere.

Without the token the answer is `401`. Each read is one request, so it wakes a sleeping instance like any other; counters start from zero whenever the instance restarts.

## Rolling back

Render keeps previous deploys. In the dashboard: `goflow-api` → **Deploys** → pick the last good one → **Rollback**. A rollback doesn't undo migrations, so a migration must keep working with the code before it (add columns before using them, drop them a release later).

## Rotating the database password

If the URL may have leaked: in the Neon console, open the `production` branch's roles and reset the password for `neondb_owner`. Build the new URL as above, then update `GOFLOW_DATABASE_URL` in Render (**Environment**) and save with a redeploy. The old password stops working at once, so the API fails its database calls until the redeploy is live. Sessions and accounts are unaffected.
