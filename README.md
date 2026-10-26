# Nightscout API Compatibility Tests

This repo is designed to compare "Nightscout compatible" API's with the original nightscout to identify issues, that may be causing errors.
FWIW, I don't think that any service should replicate the original nightscout API, but replicating enough to work with most major clients, AAPS/Loop/SugarPixel should be enough.

# Setup

1. RUN `docker-compose -f nightscout.compose.yml up`: this starts an ephemeral nightscout instance that is cleaned with each restart.
2. RUN `go run main.go nightscout`: This goes through the `*.http` files and runs each in order. 

# HTTP files

The HTTP files describe a series of requests like:

```
NAME create_entries_v3
POST http://{{ .site }}/api/v3/entries
B {"sgv": 100, "type": "sgv", "app": "test-app", "dateString": "{{ time 0 }}" }
H Authorization: Bearer {{ jwt }}
```

It uses Go templates to replace parts of the request, e.g. `{{ jwt }}` replaces with jwt token, and `{{ time 0 }}` replaces with a time string `0` minutes ago.
`{{ itime 5 }}` is epoch milliseconds 5 minutes ago and `{{ httptime -60 }}` is an HTTP date an hour from now, for headers like `If-Modified-Since`. `{{ tokenByName .subjects "read" }}` finds a subject's access token.

You can also reference previous requests output like:

```
NAME get_entries_v3
GET http://{{ .site }}/api/v3/entries/{{ dig "create_entries_v3.identifier"  }}
H Authorization: Bearer {{ jwt }}
```

Where `{{ dig "create_entries_v3.identifier"  }}` will do a json search through the previous output to get the identifier.

# Socket.io (`.sio`) files

Nightscout also pushes data to clients over [socket.io](https://socket.io) (v4). The `requests/socket_*.sio` files describe socket.io conversations with a DSL that extends the HTTP one. They run after the `.http` files, share the same context (`.site`, `token`, `jwt`, ...) and results are written to `results/<name>/<file>/` the same way.

```
NAME storage_connect
CONNECT http://{{ .site }}/storage

NAME storage_subscribe
EMIT subscribe
B {"accessToken": "{{ token }}", "collections": ["treatments"]}
ACK

NAME storage_create_treatment_v3
POST http://{{ .site }}/api/v3/treatments
H Authorization: Bearer {{ jwt }}
B {"eventType": "Note", "app": "socket-test", "date": {{ itime 0 }}}

NAME storage_create_event
WAIT create
T 30s

NAME storage_disconnect
DISCONNECT
```

Steps:

| Verb | Meaning |
| --- | --- |
| `CONNECT <url>` | Open a socket.io connection. The URL path is the namespace (`/`, `/storage`, `/alarm`), the socket.io path is always `/socket.io/`. `H` lines are sent with the websocket handshake, an optional `B` is the namespace auth payload. |
| `EMIT <event>` | Send an event. Each `B` line is one JSON argument. Add `ACK` to wait for the server's acknowledgement, the result is what the server acked with. |
| `WAIT <event>` | Wait for the server to send `<event>`. Every event received on a connection is queued, `WAIT` takes the oldest matching one, so it can come before or after the `WAIT`. If the server kicks the client out of the namespace, a `disconnect` event is queued. |
| `COLLECT <duration>` | Wait for e.g. `2s` then record every event still queued. |
| `DISCONNECT` | Close the connection. |
| `GET`/`POST`/.../`SET` | Exactly as in `.http` files, so a script can change data and then `WAIT` for the push. |

Modifiers: `C <connect step name>` picks the connection (default: the most recent `CONNECT`), `T <duration>` sets the timeout for `CONNECT`, `ACK` and `WAIT` (default `10s`).

The result of a step (the ack or event arguments, a single argument is unwrapped) is stored in the context like HTTP responses, so `{{ dig "ws_db_add_treatment.0._id" }}` works.

Socket.io has no status codes, so results get `200` on success, `408` on a timeout and `500` for any other error (e.g. the service has no socket.io), with the message in `error`. The result `url` is `/socket.io/<namespace>/<event>` so `diff/diff.go` groups and compares them like HTTP endpoints:

```
EMIT         /socket.io/authorize            Status Score 100.00   Body Score 100.00   Overall Score 100.00   Duration200Diff 0   Total Tests 2
WAIT         /socket.io/dataUpdate           Status Score 100.00   Body Score 100.00   Overall Score 100.00   Duration200Diff 0   Total Tests 5
WAIT         /socket.io/storage/create       Status Score 100.00   Body Score 100.00   Overall Score 100.00   Duration200Diff 0   Total Tests 1
```

Current scripts:

* `socket_v1_websocket.sio`: the original `/` namespace used by the Nightscout web client: `authorize` (including `status` and a rejected token), `clients`, `dataUpdate`, `loadRetro`/`retroUpdate`, `dbAdd`/`dbUpdate`/`dbUpdateUnset`/`dbRemove` on every collection, deduplication, read only permissions.
* `socket_v3_storage.sio`: API v3 `/storage` namespace `create`/`update`/`delete` events.
* `socket_v3_alarm_levels.sio`: API v3 `/alarm` namespace `alarm`, `urgent_alarm`, `clear_alarm` and `notification`. Alarms come from the AR2 forecast, so it deletes the last 30 minutes of entries first to control the readings.
* `socket_v3_alarm.sio`: API v3 `/alarm` namespace, announcements, subscribing and `ack`.


# Other Services
You will see the output from `nighttroop` (still in development)

You can skip the auth (since your service may do it differently and then just )

## Nocturne

1. RUN `docker compose -f nocturne.compose.yml up`: starts an ephemeral Nocturne (Postgres on tmpfs) with its gateway on `localhost:1612`.
2. RUN `go run main.go nocturne`: `requests/nocturne.http` seeds a tenant with the dev-only `seed-tenant` endpoint (Nocturne has no `API_SECRET`), then creates the `write`/`read` subjects with the owner's session and swaps their tokens for JWTs, like `nightscout.http`.


# Comparison

If you run:

```
go run diff/diff.go results/nightscout results/nighttroop
```

Every result in the first folder (the reference, Nightscout) is compared with the result with the same group and name in the second. The scores are grouped by METHOD and PATH, followed by the tests that differ the most and exactly how they differ, e.g.

```
GET      /api/v3/profile        Status Score 100.00   Body Score 72.46    Overall Score 91.74    Duration200Diff -34    Total Tests 17
PUT      /api/v3/profile        Status Score 0.00     Body Score 33.85    Overall Score 10.15    Duration200Diff 0      Total Tests 2
POST     /api/v3/treatments     Status Score 58.89    Body Score 56.48    Overall Score 58.17    Duration200Diff -36    Total Tests 36
...
TOTAL                           Status Score 89.19    Body Score 68.20    Overall Score 82.89    Duration200Diff -683   Total Tests 259

Missing results (37, not scored):
  socket_v1_websocket: 19

Worst tests (lowest score first, -details for all):
  0.00  v1_treatments/016.get_treatments_after_delete_v1  GET /api/v1/treatments  (200 vs 404)
        status: 200 vs 404
        type $: array vs object
  87.74  v1_devicestatus/001.create_devicestatus_v1  POST /api/v1/devicestatus  (200 vs 200)
        missing $[].openaps
        missing $[].pump.status
        extra $[].date
```

Flags (before the folders):

* `-details` print every test that is not identical and every missing result
* `-worst N` number of worst tests to print (default 15)
* `-max-diffs N` differences printed per test (default 10, 0 for all)
* `-bad` also compare `x_bad_requests`

How tests are scored:

* **Status** (70% of overall): identical 100, same class (`400` vs `404`) 80, the other service succeeding where Nightscout failed 20, failing where Nightscout succeeded 0.
* **Body** (30% of overall): a structural comparison against the Nightscout body. Types matter most (`100` vs `"100"`, object vs array score 0), then keys Nightscout returns that are missing, then array lengths. Extra keys cost a quarter of a missing key. Values matter least: strings and numbers that differ (ids, dates) score 90, booleans that differ 50. If Nightscout returned text and the other service JSON, that is fine.
* Differences are listed with their JSON path. Array indexes are generalised (`$[].sgv`) so a difference in every element is listed once with a count.
* `Duration200Diff` is the other service's duration minus Nightscout's, in ms, for tests where both succeeded.
* The setup group (named after the first folder) and `SET` steps are not compared. Results the other service has no file for are listed as missing and not scored.

These numbers are just heuristics. The goal of this report is not to get services to be 100% compatible, but highlight the differences between OG Nightscout.

# Tests

```
go test ./...
```

`diff` also has fuzz tests, e.g. `go test ./diff -run XXX -fuzz FuzzCompareIsBounded -fuzztime 30s`.

# TODO

1. Have service specific read and write queries so that we can check compatibility with say AAPS. For exmaple, if AAPS uses PATCH /api/v3/treatments in a specific way, then we can encode and make sure responses are in the right shape.
2. Test Web services API, a whole other can of worms. Started with the socket.io `.sio` files, needs more coverage (alarms, `dbUpdateUnset`, devicestatus/profile pushes) and a better body comparison for pushed deltas.
