# jq queries

Send a raw UTF-8 jq program to `QUERY /db/{database}` with `Content-Type: text/plain`.

```sh
curl -i -N -X QUERY http://localhost:8080/db/users \
  -H 'Content-Type: text/plain' \
  -H 'Accept: application/x-ndjson' \
  --data-binary 'select(.age >= 30 and .age <= 40) | .name'
```

## Input and output

KantanDB runs the program on each document body, like `jq` without `-s`. It reads from one Pebble snapshot in document-ID order. The input contains no server IDs or revisions. For example, `.` returns every document and `.skills[]` emits each skill separately. A program can use `input` or `inputs` to consume later documents from the same scan.

By default, KantanDB buffers all jq outputs in one JSON array. `Accept: application/x-ndjson` streams one compact JSON value per line. An array from jq stays on one line. If jq emits no values, JSON returns `[]` and NDJSON returns an empty body. If `Accept` excludes both formats, KantanDB returns `406 Not Acceptable`.

## Stream completion

NDJSON responses announce `Trailer: Query-Status`. Read the body to the end and check the trailer. `Query-Status: ok` confirms completion. `Query-Status: error` means the stream failed and can contain partial results. If a proxy or client drops the trailer, treat completion as unconfirmed. If the query fails before streaming starts, KantanDB returns an HTTP error with a JSON body.

## Limits

KantanDB accepts one query at a time. The program can contain up to 64 KiB of source, subject to the server request limit. JSON mode allows up to 10,000 input documents, 64 MiB of input, 8 MiB of output, and five seconds for the query. NDJSON has no total count or byte limit. It has a five-second deadline for each document evaluation and each write.

Restrict QUERY to trusted clients. Deadlines do not cap allocations. For example, `[., inputs]` collects all remaining documents in memory. KantanDB uses [gojq](https://github.com/itchyny/gojq#difference-to-jq) and does not expose local modules or environment variables. QUERY does not accept URI query parameters, use secondary indexes, or return cursors.
