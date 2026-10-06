# Ticket classification service

Ingests support tickets over HTTP, classifies them with an LLM outside the
request, and serves the results back.

Go, with one dependency: a pure-Go SQLite driver.

## Running it

```sh
docker compose up --build
```

That's it. It builds, listens on http://localhost:8080, loads the ten sample
tickets from `testdata/tickets.json` and classifies them in the background over
the next few seconds.

If 8080 is taken, use `PORT=9000 docker compose up --build`.

```sh
curl localhost:8080/tickets | jq            # watch pending become classified
curl localhost:8080/tickets/t-1005 | jq     # the prompt-injection ticket
```

Without Docker, `go run ./cmd/server` does the same. Needs Go 1.23+, no CGO.

Tests:

```sh
go test -race ./...
```

They take a few seconds and don't need anything running.

## The model

There's no real provider and no API key in this repo. `classify.Fake` stands in.
It returns plausible JSON most of the time. The rest of the time it returns one
of the things real models actually do: prose instead of JSON, a category outside
the allowed set, truncated output, an invented extra field, a "one-sentence
summary" running to six paragraphs. Sometimes the call just fails. It's seeded,
so runs are reproducible.

You can tune it, which is the fastest way to watch the lifecycle work:

```sh
# fails most calls: watch tickets retry, then fail
go run ./cmd/server -llm-failure-rate=0.8

# slow: tickets visibly sit in pending
go run ./cmd/server -llm-latency=3s

# never wrong
go run ./cmd/server -llm-failure-rate=0 -llm-malformed-rate=0
```

Using a real model means implementing one method:

```go
type Provider interface {
	Complete(ctx context.Context, p Prompt) (string, error)
}

type Prompt struct {
	System string // our instructions. Constant, never contains ticket text
	User   string // the ticket, untrusted in full
}
```

The two halves are separate because every chat API has distinct system and user
channels, and content arriving in the user channel is not an instruction no
matter what it says. Handing a provider one concatenated string throws that
away and leaves a delimiter as the only thing between our instructions and the
customer's words, which is a convention the model may or may not honour. An
implementation must map the halves onto its provider's roles rather than
joining them back together.

Nothing downstream changes, and nothing downstream trusts it more than it trusts
the fake.

## API

| | |
|---|---|
| `POST /tickets` | ingest. 201 created, 200 already known |
| `GET /tickets/{id}` | one ticket, 404 if unknown |
| `GET /tickets` | list. `?category=` `?priority=` `?status=` `?review_required=` `?limit=` `?cursor=` |
| `GET /healthz` | liveness |

```sh
curl -X POST localhost:8080/tickets \
  -d '{"id":"t-2001","subject":"Charged twice","body":"Two charges of 49.00."}'
```

```json
{
  "id": "t-2001",
  "subject": "Charged twice",
  "body": "Two charges of 49.00.",
  "status": "pending",
  "classification": null,
  "created_at": "2026-10-05T08:31:00Z",
  "updated_at": "2026-10-05T08:31:00Z"
}
```

`classification` is null until the ticket is classified, and complete once it is.
The three fields get written in one statement, so you never see a ticket that's
half-classified.

`review_required` is always present, so a caller deciding whether to trust a
classification never has to know that a missing field means no. It's set at
ingest from the ticket text, so it's known before the model is called. The
reasons behind it aren't exposed.

The attempt count isn't in the response. It describes the retry policy rather
than the ticket, and exposing it would mean changing `-max-attempts` is a
breaking change for callers. It's in the store and the logs, where it's needed.

Errors all look the same, so a caller can switch on `code` without reading the
message:

```json
{"error": {"code": "invalid_filter", "message": "category \"refund\" is not one of [billing technical account other]", "request_id": "797799062396124b"}}
```

Every response carries `X-Request-Id`, and errors repeat it in the body. If the
caller already sent one we keep it, so a trace survives the hop; otherwise we
mint one. It goes on the access log line and on anything logged while handling
the request, which means someone reporting a failure can quote one string
instead of a timestamp and a description of what they were doing.

## Decisions

These are the questions the brief left open.

### Storage: SQLite, one file

A store that forgets everything on restart would make most of the questions
below impossible to demonstrate, and surviving a restart was the part I wanted
to show working rather than describe. SQLite gives durability and transactions
without another service to run, so `docker compose up` stays one command. The
driver is `modernc.org/sqlite`, which is pure Go, so the image ends up a static
binary on Alpine with no CGO.

WAL mode matters here. It allows one writer at a time but any number of
concurrent readers, so the connection pool is sized at 8 rather than pinned to a
single connection, and `busy_timeout` absorbs writer contention instead of
surfacing `SQLITE_BUSY`.

I had that pinned to one connection first, on the reasoning that SQLite
serialises writers anyway. That was wrong, and measuring it is what showed why.
Serving reads under four concurrent writers, which is what this service actually
does:

| | pool of 1 | pool of 8 |
|---|---|---|
| `Get` under write load | ~50 µs | ~19 µs |

Reads were queueing behind the workers' writes for no reason. `List` is about
11% slower with the larger pool, which is a fair price for 2.7x on the read path.
`BenchmarkGetUnderWriteLoad` in `internal/store` is the measurement.

For scale, from `go test ./internal/store -bench=.`:

| operation | |
|---|---|
| `Create` | 117 µs |
| `Claim` + `SaveResult`, the two durable writes per ticket | 640 µs |
| `Get`, uncontended | 21 µs |

Against a model call of several hundred milliseconds, classification is
model-bound by about three orders of magnitude. The database is nowhere near
being the constraint.

### Async work: a pool over the tickets table

No in-memory queue, no broker. A fixed pool of goroutines claims one pending
ticket at a time straight from the table. The queue is the table.

Everything below falls out of that cheaply. A broker would be right at a size
where classification needs to scale separately from ingest, or where other
systems want the events. Here it would be a second thing to run, a second thing
to get wrong, and a second place for a ticket to exist in a state the database
disagrees with.

Ingest nudges the pool directly so a new ticket starts classifying straight
away instead of waiting for a poll. Missing a nudge costs one poll interval
(250 ms), not a stuck ticket.

### Concurrency: four, and it's a real limit

`-workers=N`. Pool size is the limit because a worker holds exactly one ticket
at a time and nothing starts a classification outside a worker.
`TestConcurrencyIsBounded` checks this rather than assuming it. With a real
provider you'd tune this to its rate limit.

### Restart: the lease

Claiming writes a lease expiry onto the row in the same statement that selects
it:

```sql
UPDATE tickets SET lease_expires_at = ?, attempts = attempts + 1
 WHERE id = (SELECT id FROM tickets
              WHERE status = 'pending'
                AND (lease_expires_at IS NULL OR lease_expires_at <= ?)
              ORDER BY created_at, id LIMIT 1)
RETURNING ...
```

Two workers can't take the same ticket. A worker that dies holding one doesn't
strand it, because the lease runs out and someone else picks it up. Nothing in
memory needs to survive, since nothing important was in memory.

The other half: every terminal write is conditional on the ticket still being
pending. If a lease expires while a slow classification is still running and a
second worker also produces a verdict, the second write does nothing. So under
bad timing a ticket can be processed twice, but it can only land once, and it
can't be lost.

Graceful shutdown covers the normal case. This was the extra I picked. On
SIGTERM the server stops accepting requests, then the pool stops claiming. A
worker mid-classification gets `-shutdown-grace` (5s) to finish and record its
answer. It isn't cancelled immediately, because a call you've already paid for
is worth keeping. If it can't finish in time it hands the ticket back as pending
and immediately claimable. `docker compose stop` goes down this path.

The limit, honestly: after an unclean kill (`kill -9`, OOM) the one ticket that
was in flight waits out the rest of its lease before anyone retries it. That's
up to two minutes by default, tunable with `-lease`. Recovery is bounded but not
immediate. Clearing stale leases at startup would fix it, but it is only safe if you can
prove no other instance is running. Doing that properly needs an instance
identity on the lease, which I judged past the time budget.

Nothing is lost and no ticket gets two verdicts, so this is a delay rather than
a defect. Worth being precise about why I did not just fix it anyway: today SQLite sits on
a local volume, so a second pod would get its own database and the question
could not arise. The reason to avoid the shortcut is that it bakes a
single-instance assumption into the lifecycle, and that assumption breaks
silently the first time this runs on Postgres behind more than one pod. On
Kubernetes that is the normal way to scale, so the shortcut would be a trap
rather than a saving.

### Retries: three attempts, then failed, with the reason

Two things are retryable and get treated the same way. The call failed (timeout,
rate limit, provider down), or the call worked but returned something we won't
store.

The second one is the interesting case. A malformed response isn't a permanent
fact about the ticket, it's one bad sample, and the next one is often fine. So
invalid output gets retried rather than failing on the spot.

Backoff doubles from 500 ms. It's implemented by releasing the ticket with a
future `lease_expires_at`, the same mechanism as the lease, so there's no
sleeping goroutine sitting on a ticket and backoff survives a restart for free.

After `-max-attempts` (3) the ticket goes to `failed` with the reason in `error`,
visible over the API. A failed ticket carries no classification at all, not a
partial or guessed one.

### Prompt injection

`t-1005` is the one that's read about this. What I do, roughly in order of how
much it's actually worth:

Nothing the model returns is trusted. `classify.Validate` is the only way to get
a `ticket.Classification`, which is the type the store accepts. Category and
priority have to be exact members of the allowed sets. JSON is decoded with
`DisallowUnknownFields` and rejected if anything trails it. The model can't write
a field that doesn't exist or a value outside the enum.

An unknown category is rejected, not mapped to `other`. Coercing is the
comfortable option and the wrong one: it hides model drift and successful
injection equally well, and makes the service look healthier than it is.

The summary is the one free-text field the model controls, so it's the one place
an injection can still write. It gets flattened to a single line, stripped of
control characters and capped at 300 bytes. It can't affect routing and it can't
be a wall of text on an agent's screen. Escaping at render time is still the
renderer's job.

Our instructions and the ticket never share a channel. `Prompt` carries a
`System` half built entirely from constants and a `User` half holding the
ticket, so no delimiter convention has to hold for customer text to stay out of
the instruction channel. `TestSystemPromptNeverContainsTicketText` enforces it.

Inside the user half the ticket is still fenced, labelled untrusted, and run
through `redactDelimiters` so it can't close the fence around itself. That stops
a body passing part of itself off as the subject. The wording also says that
claims about the sender are themselves content, which is the weakest layer here
and is defence in depth rather than a control.

On top of that, ingest flags tickets that appear to be addressing the
classifier rather than describing a support problem. `classify.Suspicious` scans
the subject and body for a narrow set of patterns: overriding earlier
instructions, reassigning the model's role, naming a category or priority to
use, shaping the output format. It runs on the text alone, before any model
call, and it folds the cheap evasions first, so zero-width characters, smart
quotes, odd casing and line breaks mid-phrase don't get past it.

A flagged ticket is still classified. Flagging is not rejecting: t-1005 is a
real customer with a real question underneath, and dropping it would lose that.
What changes is that the result is marked `review_required` and can be listed
with `?review_required=true`, so the misrouting stops being silent and a human
decides.

The reasons behind a flag are logged and stored but never returned over the API.
Telling a caller which rule caught them is a free evasion guide.

What this still doesn't do, which is the part worth being straight about: it
cannot stop injection, only notice the recognisable shapes of it. The sample
injection asks for `technical` and `high`, both legal values, so validation on
its own passes it, and a sufficiently reworded attempt gets past the patterns
too. `TestInjectionSurvivesValidationWhenItAsksForLegalValues` asserts the first
half of that, and the fake provider is credulous enough to obey an injection on
purpose, because a fake that always resisted would let a broken defence pass the
suite.

So the honest statement of the guarantee is three parts. An injection cannot
escape the schema or write anything the schema disallows. It can still influence
how a ticket is routed. When it uses a recognisable phrasing, that influence is
flagged rather than silent, and the false positive rate on the sample data is
zero. Closing the rest is a detection problem rather than a parsing one, and the
next step is a model-based check rather than more patterns.

Ticket bodies are stored verbatim. Sanitising at ingest would destroy evidence,
and t-1005 is a real customer who really does want to know where to download
their invoices.

### API shape

`POST /tickets` returns 201 for a new ticket and 200 for one we already had.
Re-submitting isn't an error, since an upstream mail poller retrying is the
expected cause, but the caller can still tell which happened. A duplicate
doesn't queue classification again.

An unknown filter value is a 400 rather than an empty page. A typo in
`?category=` should be loud, and an empty list looks like a legitimate "no
results".

Pagination is keyset on `(created_at, id)` rather than offset, so a ticket
arriving mid-traversal can't shift a page boundary and make a caller skip a row.
Cursors are opaque and a forged one is rejected.

There's no total count. Returning one means `COUNT(*)` over every matching row
on each request, which is the only part of an otherwise page-sized endpoint that
would cost the whole table. Callers follow the cursor until it stops coming
back, and that's enough.

## Layout

```
cmd/server/        flags, wiring, shutdown sequence
internal/ticket/   domain types and the allowed sets. Depends on nothing
internal/classify/ the model boundary: prompt, validation, the fake provider
internal/store/    SQLite: idempotent create, atomic claim, conditional writes
internal/worker/   the pool: retries, backoff, graceful shutdown
internal/httpapi/  handlers, filters, error format
```

Dependencies point one way, into `ticket`. The rule I followed: a
`ticket.Classification` can only be built by `classify.Validate`, so holding one
is itself proof the model's output was checked.

`worker` and `httpapi` each declare the slice of the store they need as an
interface, next to the code that uses it, rather than taking `*store.Store`.
Four methods for the pool, three for the handlers. `cmd/server` knows the
concrete type, which is where that knowledge belongs.

## Tests

48 functions, 129 cases counting subtests. What they're for:

`classify` holds the table of responses that must never reach the store: prose,
bad enums, truncated JSON, unknown fields, two objects, oversized summaries,
wrong case. Plus the injection test above, which asserts the gap instead of
papering over it.

`store` has idempotent create, including that re-ingesting doesn't reset a
classified ticket. Eight goroutines race to claim fifty tickets and every ticket
must be claimed exactly once. Lease expiry. A late second verdict doing nothing.
Keyset pagination covering every row once.

`worker` covers retry-then-fail with the attempt count checked, recovery from a
transient failure, a confidently wrong model leaving nothing at all in the store,
shutdown handing back work it can't finish and keeping a verdict it can, and the
worker limit being real.

`httpapi` covers idempotency including that a duplicate doesn't re-queue
classification, validation, injection content stored verbatim, and filters and
paging end to end. It also pins the response schema: the exact key set for a
ticket in each of its three states, the nested classification, the list
envelope and the error envelope. Asserting the whole set catches a field added
by accident as well as one quietly dropped, which is why it isn't a set of
checks for individual fields.

The two I'd least like to lose are `TestClaimIsExclusive` and
`TestTerminalWritesHappenOnce`. They're the properties the whole lifecycle rests
on and both would break quietly.

## Scaling

One process, one file, on purpose. Here's what I'd do past that and what would
actually trigger each step. Written down because I considered these and decided
against them, which isn't the same as not thinking about it.

What it handles now: four workers against a one-second model call is about four
tickets a second, call it 300,000 a day. A support desk producing that has a
bigger problem than ticket routing. So throughput isn't what moves you off this
design, availability is. The real limits are that a deploy means a gap in
classification and that one process is one thing to lose.

First, authentication and rate limiting. The trigger is the service being
reachable by anything other than our own ingest path. Today it sits behind
whatever puts tickets into it, and every caller is trusted. The moment it takes
traffic from outside that, it needs an API key or mTLS on write, and a per-caller
rate limit. Ingest is the endpoint to protect: it is unauthenticated, it writes,
and each accepted ticket costs a model call, so an open ingest is a way to spend
our money. Request ids are already in place to attribute that traffic.

Second, more than one instance. You want this for zero-gap deploys and for not
having a single point of failure, not for throughput. That's the move to
Postgres, where the claim becomes `SELECT ... FOR UPDATE SKIP LOCKED`.

This is the ceiling on the current design, and it is worth naming exactly: the
lease model is already safe for several instances sharing one database, but
SQLite on a local volume is not shareable, so today the architecture scales up
rather than out.

Worth being specific about what the move costs: nothing in the lifecycle logic
changes. The lease lives in the row rather than in memory precisely so the
correctness argument survives the migration. The claim is still atomic, terminal
writes are still conditional on pending, an abandoned ticket is still recovered
by expiry. New implementation of `worker.Store` and `httpapi.Store`, one new
service.

Third, a broker, and only for the right reason. Redpanda or Kafka earns its
place when classification has to scale separately from ingest, when ingest
arrives in bursts that need buffering, when other systems want the same events,
or when you need replay. None of that is true here.

The cost is easy to miss: a broker doesn't replace the database. Tickets still
need somewhere `GET /tickets` can read them, so a ticket would live in a topic
and in a table, and the two can disagree. That's a dual-write problem this design
doesn't have, since the queue is the table. Doing it properly means an outbox,
which is why a broker is third rather than first.

`internal/store` is the seam for all of it. The lifecycle, the retry policy and
the model boundary are untouched by any of these steps.

## Weaknesses

- Injection detection is pattern matching, so a reworded attempt gets past it.
  It narrows the silent case rather than closing it, and a model-based check is
  the next step.
- The lease and the call timeout are separate flags that have to agree. Setting
  a call timeout longer than the lease would have the model called twice for the
  same ticket. Only one verdict can land, so it was never a correctness bug, but
  it is duplicated spend, and it was possible to configure until I enforced the
  floor in `worker.New`. Two knobs with an invariant between them is a worse API
  than one derived from the other, which is what I would do with more time.
- No re-classification. `prompt_version` is stored on every classified ticket,
  so finding the ones classified under an older prompt is a query, but there is
  nothing to act on it. That was the deliberate cost of picking graceful
  shutdown as the one extra.

## With more time

Replace the injection patterns with a model-based check. A second pass asking
only "does this content try to instruct you?" catches rewordings that fixed
patterns can't, and feeds the same `review_required` flag. It costs a second
model call per ticket, which is why the cheap version shipped first.

An eval set. Label the ten sample tickets, add a script reporting agreement, make
it a gate on prompt changes. `prompt_version` is already stored on every
classified ticket for this, which also makes "re-classify everything older than
version N" a query rather than a migration.

Operational metrics: classification latency, attempts per ticket, rejection rate
by failure kind. Rejection rate is the number that tells you a model or prompt
change has gone wrong, and right now it only shows up in the logs.
