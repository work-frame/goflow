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
| No rate limit on login | Passwords can be guessed as fast as bcrypt allows | Phase 9. Closed registration limits who has an account meanwhile |
| Free Key Value has no persistence | A restart empties both lists; queued jobs lose their IDs | The reconciler re-sends jobs `queued` for over 10 minutes, at start and hourly |
| Neon free: 100 compute-hours a month; can't stay awake for free | A query every few seconds would use the month up in about 2.5 weeks, then suspend the database | The relay never polls while healthy; the reconciler runs hourly. Watch Neon's usage page |
| Workers inside the API | One API instance only (startup recovery resets `processing` jobs); jobs run on the API's tenth of a CPU | A paid background worker and leases (Phase 12) before more instances |
| Render health checks every 5 s are logged | About 17,000 log lines a day of `/healthz` | Phase 10: skip request logs for `/healthz` |

## Queue and workers (Task 27)

Merging a change to `render.yaml` updates the Blueprint: Render creates `goflow-queue` and sets `GOFLOW_REDIS_URL` and `GOFLOW_WORKERS` on the API by itself. Check after the deploy:

1. **Dashboard:** `goflow-queue` exists, plan **Free**, region **Frankfurt**, status **Available**.
2. **API logs:** `queue_enabled=true workers=2`, `queue connected`, `worker pool started workers=2`, and `relay: jobs queued` for jobs that were waiting in the outbox.
3. **End to end:** `goflow job create hosted queue test`, then after about 5 s `goflow job list` shows it `completed`.

## Rolling back

Render keeps previous deploys. In the dashboard: `goflow-api` → **Deploys** → pick the last good one → **Rollback**. A rollback doesn't undo migrations, so a migration must keep working with the code before it (add columns before using them, drop them a release later).

## Rotating the database password

If the URL may have leaked: in the Neon console, open the `production` branch's roles and reset the password for `neondb_owner`. Build the new URL as above, then update `GOFLOW_DATABASE_URL` in Render (**Environment**) and save with a redeploy. The old password stops working at once, so the API fails its database calls until the redeploy is live. Sessions and accounts are unaffected.
