# Contact form example (Task 89)

A contact form whose backend hands each message to GoFlow as a webhook job, using an API key that never leaves the server. It shows GoFlow's API keys and webhook delivery working together, from the outside: it uses only GoFlow's HTTP API and Go's standard library.

**This is an integration example, not a contact-form service.** It listens on this computer only (`127.0.0.1`), stores nothing itself, and has no spam protection beyond a hidden field. Use made-up details only (see [Where the data goes](#where-the-data-goes)).

```text
browser ──form POST──► this backend ──POST /api/v1/jobs, Authorization: Bearer gfk_…──► GoFlow
                                                                                          │ webhook job, signed
                                                                                          ▼
                                                                          the receiver (goflow-receiver /hook)
```

## Running it

1. In the GoFlow dashboard: **API keys** → **Create key**. Name `contact form`, scope **jobs:create only**, 30 days. Copy the key.
2. In a GoFlow checkout, set it without echo, so it isn't on screen or in your shell history:
   ```bash
   read -rs CONTACT_GOFLOW_KEY && export CONTACT_GOFLOW_KEY
   ```
   Paste the key and press Enter.
3. Start the backend:
   ```bash
   go run ./examples/contactform
   ```
4. Open http://127.0.0.1:8090 and send a made-up message (for example `Test Person`, `test@example.com`).
5. When you're done: stop it with Ctrl+C, `unset CONTACT_GOFLOW_KEY`, and revoke the key in the dashboard.

The account needs a webhook signing secret, and the receiver must hold the same one (`RECEIVER_SECRET` on Render, Task 67). If they differ, the job's attempts show `401` from the receiver.

| Setting | Default | Rule |
|---|---|---|
| `CONTACT_GOFLOW_KEY` | (required) | a whole `gfk_` key; only ever sent in the `Authorization` header to `CONTACT_GOFLOW_URL` |
| `CONTACT_GOFLOW_URL` | `https://goflow-api-hewh.onrender.com` | `https://`; `http://` only for `127.0.0.1` or `localhost` |
| `CONTACT_WEBHOOK_URL` | `https://goflow-receiver.onrender.com/hook` | `https://`; fixed when the backend starts |
| `CONTACT_ADDR` | `127.0.0.1:8090` | loopback addresses only |

## What the backend decides, and what the visitor may

The visitor chooses three values: name, email and message. Everything else in the job is the backend's: the type (`webhook`), the title (`contact form message`, the same for every job, because titles appear in job lists and live events), the webhook URL, the body's shape and `max_attempts` (5). Extra form fields and query parameters are ignored.

That boundary is the point of the example. A `jobs:create` key can choose any webhook URL and any body, and GoFlow signs whatever it is given with the account's secret. So a key in a browser would let anyone send correctly signed messages to the real receiver, which would accept them: the signature proves "sent through this GoFlow account", not "sent by this form". Kept on the server, the key can only make the job this program builds.

The receiver gets:

```json
{"submission_id": "…", "name": "Test Person", "email": "test@example.com", "message": "…"}
```

`submission_id` is the form's token ID. There is no send time in the body on purpose: a resend of the same page must ask GoFlow for exactly the same work (or it answers `422`), and a time would differ. The job's `created_at` and the `webhook-timestamp` header say when.

## One click, one message

Each form page carries a token: 16 random bytes and an HMAC of them, under a key the backend makes when it starts. The backend sends `contactform-<token ID>` as GoFlow's `Idempotency-Key`, so sending the same page twice (a double click, Back and send again, a retry after an error) makes one job: GoFlow answers the second time with the first job and `Idempotent-Replayed: true`. A new page gets a new token.

Only tokens this process signed are accepted. A visitor can't choose the key, another website can't post the form on someone's behalf (it can't read the token from the page), and after a restart old pages say "reload".

That stops duplicate jobs. Duplicate deliveries are the receiver's to stop: GoFlow delivers at least once, and the receiver remembers each `webhook-id` it processed.

## What the visitor sees

| What happened | The page says |
|---|---|
| GoFlow made the job (`201`), or had made it already (`200`, a replay) | "Thanks, your message was received." That means accepted, not delivered: delivery happens after this page |
| The hidden field was filled (a bot) | the same thanks page; nothing is sent |
| The token isn't one this backend signed | "This form has expired" |
| A field is wrong | the form again, with the reason |
| GoFlow `422` (the same page sent again with different text) | "already sent with different text; reload the page" |
| GoFlow `429` | "Too many messages right now"; the form keeps the text and the token |
| Any other answer, no answer within 10 s, or no connection | "couldn't be sent; try again"; the form keeps the text and the token, so a retry can't make a second job |

GoFlow's own answer is never shown and never logged. The log has one line per send: `message accepted job_id=… replayed=…`, `goflow refused status=…`, `goflow unreachable`, `bad token` or `honeypot: dropped`. No name, email, message or key.

Limits: name 1–100 characters, message 1–2,000 (counted in characters, so `Ọlá` is 3), email at most 254 with one `@` and a dot in the domain. Control characters, invalid UTF-8 and bidirectional overrides are refused; messages keep newlines and tabs. The request body is capped at 32 KiB, GoFlow's answer is read up to 64 KiB, redirects from GoFlow aren't followed, and the server has read, write, header and idle timeouts.

## Where the data goes

Whatever is typed into the form is stored by GoFlow, not just passed through:

- in the job's payload in PostgreSQL, returned by `GET /api/v1/jobs/{id}` to the account (not to this key: it lacks `jobs:read`), until the job is deleted;
- in every nightly backup made while the job existed, which cannot be deleted for 35 days ([DEPLOYMENT.md](../../DEPLOYMENT.md#automated-backups-phase-18));
- at the receiver, which here logs only the ID and the body's size.

A real contact form would need a decision on how long messages are kept and who may read them, a way to delete them (deleting the job removes the payload from the database, not from backups already made), and its visitors told. This example avoids all of that by using made-up data only.

## Tests

`go test ./examples/contactform` runs against a fake GoFlow that records every request: the exact job (headers, type, title, URL, body, `max_attempts`), that extra fields can't change it, that resending a page reuses its key, that forged tokens, the honeypot and invalid fields send nothing, that every GoFlow failure shows fixed words and never GoFlow's text, that the key never reaches a page, a header or the log and submitted text never reaches the log, that redirects aren't followed and huge answers are cut off, and that bad settings are refused. The live run against the hosted API is recorded after it has been done (Task 89, second commit).
