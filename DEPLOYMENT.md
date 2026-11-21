# Deployment

GoFlow's API runs on [Render](https://render.com) (free web service) with its database on [Neon](https://neon.com) (free PostgreSQL). This file records how it is set up, what was measured, and what the free plans can't do.

Free hosting is for a first deployment and for testing. It is not yet a service people should depend on (see [Limits](#limits-of-the-free-setup)).

## Layout

```text
goflow CLI ──HTTPS──► Render edge ──► goflow-api (Render, Frankfurt) ──TLS verify-full──► Neon PostgreSQL 17 (AWS Frankfurt)
```

| Part | Where | Settings |
|---|---|---|
| API | Render web service `goflow-api`, free plan, region `frankfurt` | 0.1 CPU, 512 MB. Docker runtime since Task 45: Render builds the repo's [`Dockerfile`](Dockerfile), as set in [`render.yaml`](render.yaml) |
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

After that, every merge to `main` deploys automatically once its CI checks pass (`autoDeployTrigger: checksPass`, Task 60; before that, `commit`: on every merge).

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
| Workers inside the API | Jobs run on the API's tenth of a CPU, in one instance (the free plan has no background workers) | Leases (Task 48) make more instances or a paid background worker safe to add |
| The live-delivery canary publishes every 2 seconds | About 43,000 Redis commands a day; outbound only, so it doesn't keep the instance awake | Render's Key Value docs mention no command quota. Lengthen the interval on a per-command plan |
| Metrics aren't collected on Render, only read by hand | No history or alerts; counters restart with the instance | Prometheus and Grafana run locally (compose profile `monitoring`). A hosted collector when the instance is paid and always on anyway |

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

Result on 5 November 2026 (deploy `857ea6d`, Task 41), after one `flaky:1` job created in the dashboard:

| Line | Value | Why |
|---|---|---|
| `goflow_job_status_changes_total` | pending 1, processing 2, retrying 1, completed 1; no `queued` | one failed attempt, one success; a worker claimed the job before the relay marked it queued |
| `goflow_job_attempt_duration_seconds` | `retry`: 1 attempt, 7 µs; `completed`: 1 attempt, 1.001 s | `flaky` fails at once and succeeds after 1 s of work |
| `goflow_events_published_total` / `_dropped_total` | 5 / 0 | one event per status change |
| `goflow_event_streams` | 1 | the dashboard tab |
| `goflow_queue_jobs` | 0, 0, 0 | finished; the scrape reached Key Value |
| `goflow_rate_limit_fallback` | 0 | the limiter counts in Redis |

`process_start_time_seconds` showed an instance 64 seconds old: the free instance had spun down after 15 idle minutes, and the login woke a new one. An earlier read on the old instance had shown no job lines at all.

## Delivery health (Task 42)

After the deploy, with the dashboard open:

1. The dot says `live`, and the metrics (as above) include `goflow_event_delivery_ok 1`.
2. In Render's logs there is no `live delivery paused` line. If Key Value ever restarts, you'll see `events: live delivery paused; streams stay open and reload when it resumes` about 8 seconds into the outage, the dot turns to `paused: updates delayed`, and then `events: live delivery resumed` with how long it lasted.

There is no safe way to cut Render's Key Value off on purpose, so the pause itself was tested locally (see ARCHITECTURE.md, Delivery health): the dot said `paused` 8.3 seconds after Redis went away, on the same stream, and `live` 0.2 seconds after it came back.

Result on 5 November 2026 (deploy `fab1c24`): the dot said `live`, and `/metrics` showed `goflow_event_delivery_ok 1`, `goflow_event_streams 1` and `goflow_events_dropped_total 0`. `goflow_events_published_total` was `0` because no job had changed on that instance yet: canaries go straight to Redis and aren't counted as events.

## The Docker runtime (Task 45)

Since Task 45, Render builds the [`Dockerfile`](Dockerfile) and runs its image, the one `docker compose` runs locally, instead of building with its own Go toolchain. Nothing else changed: the health check, every setting and the Key Value queue are as before.

- **Settings never enter the image.** Render passes each environment variable to the build as a build argument, so a Dockerfile `ARG GOFLOW_DATABASE_URL` would bake the database password into the image's history (FAILURES.md, case 44). The Dockerfile declares no `ARG`; `TestImageHoldsNoSettingsOrSecrets` fails if one appears. The settings reach the container when it starts.
- **The switch.** Merging changes `runtime` in the Blueprint. Like in Task 38, the commit's auto-deploy may run before the Blueprint sync: then one deploy builds natively (still works), and the sync's deploy builds the image. A failed build leaves the previous instance serving.
- **Checks after the switch:**
  1. In the deploy's log: Docker build steps (`FROM golang:1.27-trixie`, `go mod download`, `go build`, `FROM gcr.io/distroless/static-debian13:nonroot`), then the build time.
  2. In the runtime log: `starting`, `database connected`, `listening` on Render's `PORT` (10000), `worker pool started`.
  3. The dashboard: log in, the dot says `live`.
  4. A `flaky:1` job: `retrying`, then `completed 2/3`.
  5. `/metrics` with the token, as in [Metrics](#metrics-task-40): `200`, `goflow_event_delivery_ok 1`, the job's status changes.
- **Going back.** Revert the Task 45 commit (render.yaml back to `runtime: go`) and merge; or roll back to a pre-Task 45 deploy in the dashboard as below, though the next Blueprint sync would switch the runtime again.

Result on 5 November 2026 (deploy `571d939`):

| Deploy | Trigger | Took | What happened |
|---|---|---|---|
| 17:35 | Auto-Deploy | 30.9 s, **failed** | Built natively with the old `go build` command. The Blueprint sync then removed the start command before the instance started: "Application exited early", and the new process logged nothing. The old instance kept serving |
| 17:36 | Blueprint | 1 min 32 s | The first Docker build on Render, including downloading `golang:1.27-trixie`. The service badge changed from `Go` to `Docker` |
| 17:46 | Manual (saving the Environment) | 24.3 s | Docker again, from Render's layer cache |

The new instances started with the same settings as before (`starting addr=:10000 ... queue_enabled=true workers=2`, `listening addr=[::]:10000`), and the old one stopped cleanly (`worker pool stopped: every active job finished`), its dashboard stream reconnecting to the new one. Checks: login and the job list; `flaky:1` completed 2/3, `flaky:3` and `flaky:5` dead 3/3; `/metrics` `200`, `goflow_event_delivery_ok 1`, and status changes matching the three jobs exactly (pending 3, processing 8, retrying 5, completed 1, dead 2; 19 events published).

Same race as Task 38: the commit's auto-deploy runs with the old settings while the Blueprint sync changes them. This time it cost one failed deploy and no downtime. A change to how the service is built or started deserves its own deploy: sync the Blueprint first, or pause auto-deploy while it lands.

## Leases (Task 48)

Migration 0010 adds `lease_token` and `lease_expires_at` and a constraint that ties them to `processing`. Two things to know when it reaches Render:

- **Deploy while no job is running.** For a few seconds the old instance (no leases) keeps serving while the new one has already applied the migration. A job the old instance claims or finishes in that window breaks the new constraint, so its write fails. Nothing is lost: the job stays `processing` (or its ID stays in Redis), the new instance's lease sweep or startup tidy-up sends it again, and it runs once more. With the dashboard showing nothing `processing`, nothing happens at all.
- **Checks after the deploy:** the startup log shows `migrations applied versions=[10]`; a `flaky:1` job still ends `completed 2/3`; `/metrics` still says `goflow_event_delivery_ok 1`. With one instance on the free plan there is nothing to take back, so the log should show no `lease reaper` lines.

Result on 6 November 2026 (deploy `3e5d521`):

| Check | What happened |
|---|---|
| Deploy | One auto-deploy, 1 min 12 s, no failed deploy. Nothing was `processing` while it ran |
| Migration | `migrations applied versions=[10]` at 15:14:52 UTC |
| A `flaky:1` job | `a3267d7f…`: `retrying`, then `completed 2/3`, the same as before leases |
| `/metrics` | `goflow_event_delivery_ok 1` |
| Local Compose stack | Rebuilt from the same commit; its API applied migration 10 at 15:24:00 UTC on the development database |

Render still runs one instance, with the API's own two workers: a separate worker service isn't on the free plan. Several processes sharing the work is tested locally instead, with the Compose stack's three worker containers (Task 49, [DEVELOPMENT.md](DEVELOPMENT.md#crash-test-kill-a-worker-mid-job)).

## Worker containers (Task 49)

Nothing changes on Render: the worker containers are part of the local Compose stack only. The one change the hosted API runs is a new log line, `worker: job claimed`, with the attempt and the lease deadline, so the log shows which process holds which job.

- **Check after the deploy:** create a `sleep:2s` job in the dashboard, then search the runtime log for `job claimed`. The line shows `attempt=1` and a `lease_expires` 30 s after its own time, followed about 2 s later by `worker: job completed` for the same `job_id`.

Result on 9 November 2026 (deploy `d4ba590`):

```text
17:49:28.664Z msg="worker: job claimed" worker=1 job_id=7eb723a138f632a94ad9fb15c4d91196 attempt=1 max_attempts=3 lease_expires=2026-11-09T17:49:58.662Z
17:49:30.685Z msg="worker: job completed" worker=1 job_id=7eb723a138f632a94ad9fb15c4d91196 attempt=1 max_attempts=3 duration=2s
```

The dashboard showed `7eb723a1` `completed 1/3`, with the dot `live`. The deadline is 29.998 s after the log line's time: PostgreSQL set it (`now()` + 30 s) a moment before the worker wrote the line.

## Deploys wait for CI (Task 60)

`autoDeployTrigger: checksPass`: Render deploys a commit on `main` only after GitHub reports its checks passed, here `check` and `govulncheck` from the push run of [.github/workflows/ci.yml](.github/workflows/ci.yml). A commit whose checks fail is never deployed. Branch protection already keeps failing code off `main`; this covers what branch protection can't see, such as a check that passed on the pull request and fails after the merge.

- **The merge that brings the setting:** like Task 38, Render may deploy that commit with the old setting (at once) before the Blueprint sync applies the new one. Expected, and harmless: the commit passed CI on its pull request.
- **Check once, after that merge:** in the dashboard, `goflow-api` → **Settings** → Auto-Deploy shows that deploys wait for checks to pass. Settings only: not the Environment page.
- **Check on the next merge:** `gh run list --branch main --limit 1` shows the push run in progress, and **Events** shows no new deploy until it is green.

Result on 14 November 2026 (merge `b27c2af`, PR #73):

- The push run on `main` started at 08:49:43 UTC. Render was already building `b27c2af` by 08:50, while that run was still in progress: the deploy started under the old setting, as expected. The run then passed.
- At 08:53 Settings → Deploy still showed **On Commit**. That page had been open since before the sync; only its `#deploy` anchor had changed, so it was never reloaded. The Blueprint listed `goflow-api` as updated around 08:51, a **Manual sync** at 08:55 found "Resources already up to date", and the reloaded page showed **After CI Checks Pass**. The automatic sync had applied it within about two minutes of the merge. Reload a settings page before trusting it.
- Render's Deploy Hook sits on the same page, masked. It is a secret: anyone with the URL can trigger a deploy. Keep it masked in screenshots.

The first merge under the new setting (`ff17817`, PR #74, 14 November 2026), times in UTC:

| Time | What |
|---|---|
| 09:00:17 | CI run on `main` starts (push) |
| ~09:00 | Events: no deploy for `ff17817` |
| 09:02:21 | CI run passes (`check` 2m1s, `govulncheck` 21s) |
| 09:02 | Render: deploy started for `ff17817` |
| 09:03 | Render: live |

The two merges before it started deploying in the minute they were merged, while CI was still running. This one waited about 2 minutes, until the run passed. Render's Events show minutes only, so "after 09:02:21" is as close as it can be measured.

## The secrets key (Task 65a)

`render.yaml` adds `GOFLOW_SECRETS_KEY` with `generateValue: true`: the Blueprint sync creates it with 32 random bytes in base64, the same way it created `GOFLOW_METRICS_TOKEN` in Task 40, and later syncs keep the value. Task 65a only adds the variable; nothing reads it yet. Task 65b makes the API and the worker require it, so it must exist before that deploy.

- **Check after the deploy:** `goflow-api` → **Environment** lists `GOFLOW_SECRETS_KEY`. Don't take a screenshot of that page. Since Task 81 the value is copied once, into the password manager, because a restored database needs it ([Saving the secrets key](#saving-the-secrets-key-task-81)); otherwise there is nothing to do with it.
- **Never delete or regenerate it.** It encrypts every user's signing secret; without it they can't be read. Rotation keeps the old key until everything is re-encrypted (documented with Task 65b).

## Webhook delivery (Task 65b)

From this deploy the API refuses to start without `GOFLOW_SECRETS_KEY` (Task 65a created it). Migration 0012 adds `webhook_secrets` and `webhook_attempts`.

- **Check after the deploy:** the runtime log shows `secrets key loaded key_id=<8 hex characters> secrets_reencrypted=0`. The key ID is a hash prefix, safe to log; the key itself never appears.
- A first webhook needs a receiver you control: Task 67.

### Rotating the secrets key

Only if it may have leaked. Never delete `GOFLOW_SECRETS_KEY` or press "Generate" on it: every signing secret would become unreadable, and the API would refuse to start (which is how you'd find out).

1. Generate a new key on your machine: `head -c 32 /dev/urandom | base64`. It prints once; don't paste it anywhere but the next step.
2. In Render, `goflow-api` → **Environment**: copy the current `GOFLOW_SECRETS_KEY` value into a new variable `GOFLOW_SECRETS_OLD_KEYS` (several old keys: comma-separated), then replace `GOFLOW_SECRETS_KEY` with the new one. Save; Render redeploys.
3. The log shows `secrets key loaded key_id=<new ID> secrets_reencrypted=<number of users with a secret>`: everything is now under the new key.
4. Then delete `GOFLOW_SECRETS_OLD_KEYS` and let it redeploy. The log shows `secrets_reencrypted=0`.

If step 2 is skipped or the old key is wrong, the deploy fails with `webhook secrets are encrypted with keys that aren't loaded (key ID: secrets): map[<old ID>:<count>]`, and the previous deploy keeps serving. Put the old key back in `GOFLOW_SECRETS_OLD_KEYS` and deploy again. Users' signing secrets don't change in a key rotation; only their encryption does.

### Users rotating their signing secret

`POST /api/v1/webhook-secret` again. The previous secret signs next to the new one for 24 hours, by PostgreSQL's clock, and the response says until when (`previous_valid_until`).

- **Jobs already queued** are signed when each attempt is sent, not when the job was created: within the 24 hours with both secrets, after them with the new one only. A receiver that hasn't switched within 24 hours rejects those attempts; GoFlow retries them like any other failure, and they end `dead` when the attempts run out.
- **The previous secret retires** at `rotated_at` + 24 hours. Nothing deletes it; deliveries simply stop using it, and the next rotation overwrites it.
- **Two rotations within 24 hours:** the second one's "previous" is the first one's new secret; the oldest stops signing at once.

## The first live webhook (Task 67)

The merge adds a second service, `goflow-receiver`, from the same image. Expect its **first deploy to fail** with `RECEIVER_SECRET is required`: a Blueprint update ignores `sync: false` variables, so the secret has to be added by hand. Paste each block on its own.

1. Log in to the hosted API with your hosted account, and make your signing secret straight into a file only you can read:
   ```bash
   export GOFLOW_API_URL=https://goflow-api-hewh.onrender.com
   ./bin/goflow login
   (umask 077; ./bin/goflow webhook-secret > ~/receiver-secret.txt)
   ```
   The secret never appears on screen; the warning does.
2. In Render, `goflow-receiver` → **Environment** → add `RECEIVER_SECRET` and paste the file's contents (open it in your editor and copy). Save: Render deploys again. Don't screenshot that page.
3. `rm ~/receiver-secret.txt`. The receiver has it now, and GoFlow can't show it again.
4. `goflow-receiver` → **Logs** shows `receiver listening`. Its URL is at the top of its page (`https://goflow-receiver-....onrender.com`).
5. Send one webhook that fails once on purpose:
   ```bash
   printf '{"order": 1042, "note": "first live webhook"}' > order.json
   ./bin/goflow job create --webhook "https://<receiver host>/hook?fail=1" --body @order.json --max-attempts 5 first live webhook
   ```
6. `./bin/goflow job attempts <job id>`, and the receiver's logs. Expected: possibly a first attempt that times out (the receiver was asleep: up to about 50 s to wake, past the 10 s limit), then `retry 503` (the planned failure), then `delivered 200`; in the receiver's log `webhook refused on purpose (?fail)`, then `webhook processed` with the same `webhook_id`.
7. Afterwards, `rm order.json`. The receiver can stay: it sleeps when unused.

Result on 14 November 2026 (deploy `40d23f0`), times in WAT (UTC+1):

```text
ATTEMPT  STARTED              TOOK   STATUS  OUTCOME    ERROR
1        2026-11-14 21:23:48  108ms  503     retry      webhook: receiver answered 503
2        2026-11-14 21:23:49  19ms   200     delivered
```

- **The `503` is the proof.** The receiver checks the signature, then the timestamp, and only then `?fail`: attempt 1 could only reach `503` with a signature made by the secret it holds and a fresh timestamp. A wrong secret answers `401`.
- **One second** between the attempts: the first backoff (1–2 s). No cold-start timeout: the receiver had just been deployed and was awake.
- **What went wrong on the way, and was caught:** the receiver's first two deploys failed with `RECEIVER_SECRET is required` (a Blueprint update ignores `sync: false`, as planned). A value made with Render's **Generate** button was refused at startup (`must start with whsec_`): it isn't a GoFlow secret, and the receiver can't use one GoFlow didn't create. And `goflow webhook-secret` was run four times: each run rotates, keeping only the one before as "previous", so a receiver given the second secret would have answered `401` once the third existed. Make one secret, paste it, then delete the file.

## Backups, restore drills and the uptime check (Phase 16)

Neon's Free plan keeps **6 hours** of history (at most 1 GB of changes): within them, a branch can be made from any past moment without touching production; after them, nothing older can be restored from Neon. So there are two drills, and an encrypted backup of your own. Record every time below: they are the first real numbers for how much GoFlow can lose and how long getting it back takes.

The passphrase for backups lives in your **password manager**, nowhere else: not in the repository, not in a file next to the backups, not in a chat. Without it a backup can't be read, by anyone.

### Drill 1: restore from Neon's history (non-destructive)

1. Note the time (UTC). In the Neon console: your project → **Branches** → **Create branch** → from a **past point in time** a few minutes ago. Don't touch the `production` branch.
2. Count production's rows, then the new branch's, with each branch's connection string (copied from the console, pasted at the hidden prompt):
   ```bash
   ./scripts/count-rows.sh
   ```
   The counts match, except anything created after the chosen moment (`newest job created` shows it).
3. Delete the drill branch in the console. Note the time again.

### Drill 2: an encrypted backup, restored

1. `docker compose up -d --wait postgres` (the restore goes into the Compose PostgreSQL).
2. Back up, pasting production's URL at the hidden prompt; gpg asks for the passphrase twice:
   ```bash
   ./scripts/backup.sh
   ```
   It writes `~/goflow-backups/goflow-<time>.dump.gpg` (folder 700, file 600), outside the repository, and prints the size, SHA-256 and seconds taken.
3. Restore it into a new local database, `goflow_restore_drill`; gpg asks for the passphrase once:
   ```bash
   ./scripts/restore-drill.sh ~/goflow-backups/goflow-<time>.dump.gpg
   ```
   It turns the dump into SQL in memory and checks all of it before PostgreSQL sees any: the dump must end with pg_dump's closing line, and the only lines removed are Neon's `ALTER COLUMN ... SET (pg_attver=N);`, each printed as `removed: ...`. Anything else unexpected stops it with nothing restored. It then loads the SQL in one transaction, checks that the foreign keys, indexes and CHECK constraints in the dump all exist in the database, and prints the row counts. Compare them with production's from Drill 1.
   To prove the passphrase comes from your password manager and not gpg's memory, run `gpgconf --reload gpg-agent` first.
4. Drop the drill database: `docker compose exec -T postgres dropdb -U goflow goflow_restore_drill`. Keep the backup file.

Run `backup.sh` regularly (weekly, and before any risky migration): what a backup can recover is only as recent as the last one.

### Uptime check

`.github/workflows/uptime.yml` asks `/readyz` every 6 hours (and on demand: **Actions** → **Uptime** → **Run workflow**). GitHub emails the repository's owner when a scheduled run fails. In Render, also turn on email for failed deploys: `goflow-api` → **Settings** → **Notifications**.

### Results

| Drill | Date | Time taken | Result |
|---|---|---|---|
| 1: Neon branch from the past | 14 Nov 2026 | not recorded | Branch from 20:15 UTC: users 1, sessions 1, jobs 16 (10 completed), secrets 1, attempts 0, migrations 12. Production: sessions 3, jobs 17 (11 completed), attempts 2. The difference is exactly what was created after 20:15: the first live webhook job, its 2 attempts and 2 logins |
| 2: backup | 14 Nov 2026, 20:58 UTC | 109 s, including typing the URL and the passphrase twice | 6,326 bytes encrypted (21,835 as SQL), file mode 600 |
| 2: restore, first try | 14 Nov 2026 | stopped at once | Failed: stock PostgreSQL rejected Neon's `pg_attver` column options, and `--exit-on-error` stopped the restore ([FAILURES.md](FAILURES.md#what-tests-did-not-catch)) |
| 2: restore, by hand, `pg_attver` lines removed | 14 Nov 2026 | 0.22 s | Every count equal to production: users 1, sessions 3, jobs 17 (11 completed), secrets 1, attempts 2, migrations 12, newest job 20:23:48 UTC |
| 2: passphrase recalled | 14 Nov 2026 | | gpg's cache cleared (`gpgconf --reload gpg-agent`), passphrase pasted from 1Password, backup decrypted |
| 2: restore with `restore-drill.sh` (Task 69b) | 14 Nov 2026 | 9 s, including typing the passphrase | 18 `pg_attver` lines removed and printed (`jobs.type`, `jobs.payload`, every column of `webhook_attempts` and `webhook_secrets`). Foreign keys 5/5, indexes 18/18, CHECKs 29/29. Rows equal to production: users 1, sessions 3, jobs 17, outbox 0, secrets 1, attempts 2, migrations 12, newest job 20:23:48 UTC |
| Uptime check, run by hand | 14 Nov 2026, 21:41:34 UTC | | `try 1: HTTP 200` (run 43644145208; the instance was awake, so no cold start was tested) |

The drill branches were deleted in the Neon console afterwards.

**First numbers.** RPO: up to 6 hours from Neon's history; past that, everything since the last `backup.sh` run. RTO: not measured. The restore itself takes under a second at this size; the time goes to the manual steps of the [disaster recovery runbook](#disaster-recovery-restoring-production-from-a-backup), estimated at 15 to 30 minutes until someone times it.

### Automated backups (Phase 18)

Phase 16's backups depend on someone remembering to run `backup.sh`. Since 21 November 2026 they run on a schedule: every night at 02:41 UTC, from the private repository [`goflow-backups`](https://github.com/work-frame/goflow-backups) (its README says what each step does and where each secret lives). Decided on 14 November 2026:

| Part | Choice | Why |
|---|---|---|
| Where it runs | A scheduled workflow in a separate **private** repository, `goflow-backups` | This repository stays public and holds no secrets. Scheduled workflows in a public repository stop after 60 days without activity |
| Encryption | [age](https://age-encryption.org), to a **public key**. The private key lives only in the password manager | Whoever steals the workflow's secrets can make new backups but can't read any |
| Database login | A dedicated read-only role | A leaked backup login can read production, but not change or drop it |
| Storage | A private Backblaze B2 bucket, outside GitHub. The workflow's key can upload but not delete. Object Lock in compliance mode keeps each file for 35 days, then a lifecycle rule removes it | A thief with the workflow's secrets can't delete the backups either. Compliance mode can't be lifted, even by the account owner |
| Checks | Each run restores the dump in a throwaway database (`restore-filter.awk` and the structure checks) before encrypting it. GitHub emails on a failed run. A heartbeat service alerts when no successful run arrives. Once a month, a drill from a downloaded backup | A failed run, a run that never starts, and a key that no longer decrypts are each caught by a different layer |

Order: the scripts read `.age` (Task 74), then a test bucket with a 1-day compliance lock (delete refused, lifecycle still removes it), then the production bucket, the role, the key and the workflow. Administrative credentials (B2 master key, Neon owner) never go into GitHub.

To make or restore an age backup by hand (`age` from your package manager, e.g. `sudo pacman -S age`):

```bash
GOFLOW_AGE_RECIPIENT=age1... ./scripts/backup.sh                     # the public key: no passphrase asked
./scripts/restore-drill.sh ~/goflow-backups/goflow-<time>.dump.age    # paste the AGE-SECRET-KEY-1... line at the hidden prompt
```

`restore-drill.sh` hands the private key to age through a pipe (`<(...)`): it is never written to disk, and it is cleared from the script's memory once the dump is decrypted. Old `.gpg` backups restore exactly as before. An age file is about the size of the plain dump (age doesn't compress; gpg does).

**How each part was checked before production relied on it** (16 to 21 November 2026):

| Check | Result |
|---|---|
| Test bucket, 1-day compliance lock | The account owner's delete in the web console was refused ("1 files were locked and could not be deleted"); an unauthenticated download got `401` |
| The web console's "Write Only" key | Turned out to grant every write capability, `deleteFiles` and the bucket's lifecycle and lock settings included. Not used: keys are made through the API with exactly `listBuckets` and `writeFiles` |
| The upload-only key, 7 checks | Upload worked; delete, list, download and a lifecycle change were refused (`401`); a lock change was refused (`403`); hide worked, but only adds an empty marker over the locked version |
| Lifecycle after the lock | Once the 1-day locks ended, the rule removed both test files and the hidden version: 0 files, 0 bytes (checked 21 November) |
| Read-only role `goflow_backup` | Created with SQL (a console-made Neon role joins `neon_superuser`). Reads every table and `pg_dump`s; INSERT, UPDATE, DELETE, TRUNCATE, ALTER, DROP and CREATE are each refused |
| Production bucket `goflow-backups-prod` | Read back through the API before its key was made: private, SSE-B2 encryption, Object Lock in **compliance** mode for 35 days, one lifecycle rule (hide after 35 days, delete 1 day later) |
| Heartbeat | A test `/fail` ping reached the inbox as a "down" email; the first successful run sent "up" |

**Results**

| What | When (UTC) | Result |
|---|---|---|
| First manual run | 21 Nov 2026 | Did not start: `gh workflow run` answered "does not have 'workflow_dispatch' trigger". `backup.yml` wasn't valid YAML, so GitHub had ignored it ([FAILURES.md](FAILURES.md#what-tests-did-not-catch)). Fixed in `goflow-backups` (Task 83) |
| First backup | 21 Nov 2026, 20:23 | Run 44945978114, 46 s, every step passed: `goflow-20261121T202338Z.dump.age`, 20.8 KB, locked |
| First restore drill | 21 Nov 2026 | Failed before decrypting: the age private key had never been saved in the password manager ([FAILURES.md](FAILURES.md#what-tests-did-not-catch)). That backup can't be opened by anyone; it expires with its lock. A new key pair was made and its public key derived from the saved copy; the workflow uses it since `goflow-backups` Task 84 |
| Second backup | 21 Nov 2026, 20:40 | Run 44947406809, 53 s: `goflow-20261121T204002Z.dump.age`, 20,808 bytes |
| Restore drill with the real key | 21 Nov 2026, ~20:45 | Decrypted with the key pasted from the password manager. 0 `pg_attver` lines this time (18 on 14 November: Neon's dumps no longer carry them; the filter stays for older backups). Foreign keys 5/5, indexes 18/18, CHECKs 31/31. Rows: users 1, sessions 0, jobs 20, outbox 0, secrets 1, attempts 2, migrations 13, newest job 2026-11-17 20:40:56, the dashboard's newest |

**RPO now:** up to 6 hours from Neon's history; past that, up to a day (the newest nightly backup). 35 days of backups exist at any time. **RTO:** 20 min 20 s in the [timed drill](#timed-recovery-drill-task-81) (21 November 2026, to a new Neon project and a local GoFlow); a real recovery adds the Render switch, so about 25 to 30 minutes.

**Keeping it working:**

- **A restore drill on the 1st of every month** (a reminder arrives): download the newest backup from the B2 console and run `restore-drill.sh` on it. The heartbeat proves the workflow said it succeeded; only a restore proves a backup and the key still work together.
- **Secrets** are set by scripts that read the source at a hidden prompt and hand the value to `gh secret set` on its standard input, so no key is ever on screen or copied by hand. They are kept in `goflow-backups/admin/`, and running one again rotates that secret.
- **Moving the pinned GoFlow commit** (`GOFLOW_SHA` in `backup.yml`) is a deliberate change: read what changed in `scripts/restore-filter.awk` and `scripts/pgurl.sh` first.
- **GitHub disables a scheduled workflow after 60 days without activity** in its repository. The heartbeat's "down" email is what would say so.

### What the checksum proves (and what it doesn't)

Each backup run logs the file's SHA-1, as B2 records it, and `recover.sh` prints the SHA-1 of the file you downloaded. A match shows the download is the same file that run uploaded: nothing was cut off or damaged on the way. It is **not** proof the backup is genuine:

- **Anyone can make a valid backup.** The workflow encrypts to the age **public** key, which is public (it is in `backup.yml`). A forged dump encrypted to it decrypts with the real private key like any other.
- **The log is not independent.** Someone with write access to `goflow-backups` (or to the GitHub account) could change the workflow so it logs whatever they upload, or alter what a later run reports. The checksum then matches the forgery.
- **The upload key can add files.** It can't delete or overwrite a locked backup, but whoever holds it can upload a new one under a plausible name.

What does protect a restore: the dump must pass `restore-filter.awk` and the structure checks; the row counts and the newest job must look like production; and the bucket's 35-day compliance lock keeps the earlier backups unchanged, so a suspicious newest file can be skipped for an older one. Every one of these attacks needs write access to `goflow-backups`, its secrets, or the B2 account, which is why those need the strongest protection: two-factor login on GitHub and B2, the upload-only key, administrative keys never in GitHub. Real authenticity would need the backups signed with a key the workflow doesn't hold; not done.

### Disaster recovery: restoring production from a backup

For when Neon's 6 hours of history can't help: the project is gone, or the damage is older than 6 hours. `scripts/recover.sh` (Task 81) does the part that touches a database, with guards; switching Render over stays a manual step. Do it from the repository, with Docker running.

You need, from the password manager: the **age private key** ("GoFlow backup age key") and **`GOFLOW_SECRETS_KEY`** ("GoFlow secrets key"). Without the second, the API refuses to start on the restored database (`MissingKeysError`): every webhook signing secret in it was encrypted with that key.

1. **Note the time** (`date -u`): the RTO starts here.
2. **Stop the writes.** In Render: `goflow-api` → **Settings** → **Suspend Service**. The API runs the workers, so this stops both. (Not yet tried on the free plan; if the button isn't there, anything written from now on goes to the old database and is lost when you switch.)
3. **Download the newest backup** from the B2 console (`goflow-backups-prod` → Browse Files; reload the page). Note its SHA-1 from the console or the run's log ([what that proves](#what-the-checksum-proves-and-what-it-doesnt)).
4. **Make an empty database.** In the Neon console, create a **new project** in the same region (AWS Frankfurt). In **Connect**: role `neondb_owner`, pooling **off**, password hidden; copy the URL. Find its endpoint ID under **Branches** → `production` → **Computes** (`ep-...`).
5. **Restore** (`docker compose build api` first, if you will log in locally):
   ```bash
   f=$(ls -t ~/Downloads/goflow-*.dump.age | head -1) && ls -l "$f"
   ./scripts/recover.sh "$f"
   ```
   Check that `ls` shows the file you just downloaded before running the second line.
   It asks for the time from step 1, the URL (hidden), the endpoint ID, then the age key (hidden). It refuses a target that holds anything, a pooled URL, or an endpoint that isn't the one you typed, and sends nothing until the whole dump has passed its checks. The load is one transaction. Then it compares the foreign keys, indexes and CHECKs with the dump's and prints the rows per table. Answer `yes` to start GoFlow on `http://127.0.0.1:8081` against the restored database (it asks for `GOFLOW_SECRETS_KEY`): log in, see the jobs, press Enter. It prints every step's time and the RTO so far.
6. **Point Render at it.** `goflow-api` → **Environment** → `GOFLOW_DATABASE_URL` = the new URL, ending changed to `?sslmode=verify-full` ([The database URL](#the-database-url)); save, then **Resume Service**. On start the API finds every migration already applied, and the reconciler re-queues every `pending`, `queued` and `retrying` job from PostgreSQL, so the empty Valkey doesn't lose them.
7. **Check** `/readyz`, log in on the hosted dashboard, list jobs. Note the time: step 1 to here is the real RTO.
8. **Afterwards:** recreate the read-only role `goflow_backup` in the new project and give the backup workflow its URL (`goflow-backups/admin/neon-url-to-github.sh`), or the next nightly backup fails. Write down what happened in the Results table.

What comes back is the database as it was at the backup: anything later is lost, and users who logged in after it log in again.

### Saving the secrets key (Task 81)

Until 21 November 2026 `GOFLOW_SECRETS_KEY` existed only in Render's environment: had the Render service been lost with the database, every webhook signing secret in every backup would have been unreadable. Now a copy is in the password manager, checked against the live key without either being shown:

1. Render → `goflow-api` → **Environment** → `GOFLOW_SECRETS_KEY` → reveal, copy, and paste it into a new password manager item "GoFlow secrets key" (password field). No screenshot. Clear the clipboard afterwards.
2. Copy it back **from the password manager** and run:
   ```bash
   ./scripts/secrets-key-id.sh
   ```
   Paste, Enter, Ctrl+D. It prints only `key_id=` and 8 hex characters, the same ID the API computes.
3. Render → `goflow-api` → **Logs**, search `secrets key loaded`: the newest line's `key_id=` must be the same. Different: the copy is wrong; delete it and start again.

If the key is ever rotated ([Rotating the secrets key](#rotating-the-secrets-key)), save the new one the same way, and keep the old one until no backup within the 35 days needs it.

### Timed recovery drill (Task 81)

The runbook above, rehearsed without touching production: steps 2, 6 and 7 are skipped, and the target is a throwaway Neon project. The clock runs from "decide to restore" (step 1) until the restored jobs are on screen at `http://127.0.0.1:8081`. Logging in there with the real `GOFLOW_SECRETS_KEY` also proves the saved copy decrypts the restored signing secrets. Afterwards: delete the drill's Neon project (**Settings** → **Delete project**), and the local container is already gone.

**Results**

Run on 21 November 2026 from the laptop, with the newest backup at the time, `goflow-20261121T204002Z.dump.age` (20,808 bytes, SHA-1 equal to the backup run's log), into a new Neon project `goflow-drill` (Frankfurt, PostgreSQL 17). It was `recover.sh`'s first real run, Docker path included, and it worked first time.

| Step | Done (UTC) | Took |
|---|---|---|
| Decision (step 1) | 22:09 | |
| Backup downloaded from B2 (step 3) | 22:16 | ~7 min, one of them lost to a command with a `<name>` placeholder that zsh refused |
| `recover.sh` started | 22:16:28 | |
| New Neon project made, URL pasted, endpoint ID typed; target checked: 0 objects, 0 `schema_migrations` | 22:21:36 | 5 min 8 s |
| age key pasted from the password manager | 22:21:54 | 18 s |
| Whole dump decrypted and checked in memory: `removed=0 foreign_keys=5 indexes=18 checks=31` | 22:21:54 | 0 s |
| Loaded, one transaction | 22:22:11 | 17 s |
| Structure 5/5, 18/18, 31/31. Rows: users 1, sessions 0, jobs 20, outbox 0, secrets 1, attempts 2, migrations 13, newest job 2026-11-17 20:40:56 | 22:22:30 | 19 s |
| GoFlow ready on `127.0.0.1:8081`: `database schema up to date`, `secrets key loaded key_id=675f4c8f secrets_reencrypted=0` | 22:27:33 | 5 min 3 s, most of it finding `GOFLOW_SECRETS_KEY` |
| Logged in with the hosted account; the 20 jobs on screen (14 completed, 6 dead or failed, 70%, as on the hosted dashboard) | 22:29:20 | 1 min 47 s |
| **RTO (drill)** | | **20 min 20 s** |

What it showed:

- **The machine part takes under a minute.** Decrypting, checking, loading and verifying took 36 s together, and each query of those 36 s starts a new `postgres:17` container and a new TLS connection to Frankfurt. The other 19 minutes were people work: finding the file, making the Neon project, copying values, looking for a key.
- **The backup and the saved keys work together.** The age key from the password manager decrypted it, and GoFlow started on the restored data with `GOFLOW_SECRETS_KEY`: with a different key it refuses to start (`MissingKeysError`), so `secrets_reencrypted=0` with no error proves the key decrypts the restored signing secret.
- **The secrets key had one copy.** On the night it was pasted straight from Render: the password manager didn't have it yet. It was saved right after the drill and that copy checked with `secrets-key-id.sh` (`675f4c8f`). With Render gone, the drill would have stopped at that step.
- **Not covered:** steps 2, 6 and 7 (suspending Render, switching `GOFLOW_DATABASE_URL`, the redeploy) and recreating `goflow_backup`. A real recovery takes those minutes on top: expect about 25 to 30 minutes, an estimate until it happens.

**To make it faster next time:** keep the two keys as clearly named items in the password manager ("GoFlow backup age key", "GoFlow secrets key"), and note `date -u` before anything else. The command steps are in the runbook with no placeholders.

## Relay claims live (Task 72)

Phase 17 changed how the relay and the reconciler talk to Redis (no row lock held, claims with tokens, 2 s per send; [ARCHITECTURE.md](ARCHITECTURE.md#the-outbox)). Checked on the hosted API on 14 November 2026, after the merge (`61b927d`, PR #87):

- **Migration:** `time=2026-11-14T22:38:10.030Z level=INFO msg="migrations applied" versions=[13]` in `goflow-api`'s log. `job_outbox` now has `claim_token` and `claimed_until`.
- **A job end to end:** `flaky:1` (`67a2484a`), created at 22:42:23 UTC from the dashboard, reached `completed` on its second try (`2/3`). So the new path works against Neon and Render's Key Value: claim, send, finish, then the failed first attempt through the delayed set and back.

Not tested live: a Redis that hangs. That is what the tests cover (FAILURES.md, case 66); doing it on Render would mean breaking the hosted queue on purpose.

## Rolling back

Render keeps previous deploys. In the dashboard: `goflow-api` → **Deploys** → pick the last good one → **Rollback**. A rollback doesn't undo migrations, so a migration must keep working with the code before it (add columns before using them, drop them a release later).

## Rotating the database password

If the URL may have leaked: in the Neon console, open the `production` branch's roles and reset the password for `neondb_owner`. Build the new URL as above, then update `GOFLOW_DATABASE_URL` in Render (**Environment**) and save with a redeploy. The old password stops working at once, so the API fails its database calls until the redeploy is live. Sessions and accounts are unaffected.

## Resetting a password by hand (Task 78)

Until email reset exists ([README](README.md#resetting-a-password-owner-only)), `go run ./cmd/goflow-admin reset-password` sets a new password straight in the database and ends every session of that account. It needs the **owner** role's URL (`neondb_owner`): the read-only `goflow_backup` role is refused. Rehearse it on a Neon branch first; production is only touched on the day a reset is really needed.

**Rehearsal on a branch (nothing in production changes):**

1. **Before.** Neon console → **SQL Editor**, branch `production`, run this read-only query and note the result (no secret in it: a fingerprint of the hash, its cost, and the session count):
   ```sql
   SELECT left(password_hash, 7) AS cost, md5(password_hash) AS fingerprint,
          (SELECT count(*) FROM sessions s WHERE s.user_id = u.id) AS sessions
   FROM users u;
   ```
2. **Branch.** **Branches** → **Create branch** from `production` (current data). Run the query on the new branch: same result.
3. **Reset the branch.** **Connect** on the new branch, role `neondb_owner`, pooling off, password hidden; copy the URL and change its ending to `?sslmode=verify-full`. Then, in a GoFlow checkout:
   ```bash
   go run ./cmd/goflow-admin reset-password
   ```
   Paste the branch URL at the first prompt, give the email, a throwaway password twice, and `reset`. Expect `password changed for ...; N sessions ended`, N being the branch's session count.
4. **Check.** Run the query again, on the branch: a new `fingerprint` and `sessions` 0. On `production`: exactly the result from step 1.
5. **Delete the branch** in the console.

**For real:** the same as step 3 with production's owner URL. Every device, the dashboard and `goflow login`, has to log in again. Write down when and why in the Results table.
