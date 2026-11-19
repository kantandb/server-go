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

## Direct reads

For `.` and a single top-level field such as `.category`, KantanDB sends the stored JSON without running jq. A missing field returns `null`. Direct reads preserve numbers as written in storage. Wrap the query in parentheses, such as `(.category)`, to run it through jq.

## Secondary indexes

A query such as `select(.salary > 100000) | .name` can use an index on `/salary`.
The planner recognizes a leading `select` with an optional field projection.
It supports `==` for string, number, and Boolean literals and `<`, `<=`, `>`, and `>=` for string and number literals.
It also supports these predicates on top-level fields:

- `.age >= 30 and .age <= 40` scans one bounded range on `/age`.
- `.age < 30 or .age > 40` combines both scans. Every branch must have a matching index.
- `.dept | IN("hr", "sales")` looks up each literal on `/dept`.
- `.name | startswith("A")` scans the string prefix on `/name`.
- `.name | endswith("e")`, `.name | contains("ar")`, and `.name | test("^A.*e$")` scan the string entries on `/name`. KantanDB tests each indexed value before fetching the document.

KantanDB removes duplicate candidates and runs the original jq program in document-ID order.
For up to 32 lists of IDs grouped by indexed value, it can merge the lists as jq requests documents.
This lets jq stop without reading the remaining IDs.

A query such as `limit(10; (., inputs) | (select(.category >= 3) | .name)), halt` can use the same index as its `select` filter.
The limit applies to outputs, and `halt` ends the scan after the limit.
Other uses of `input` or `inputs` still use a full document scan.

If the merge does not apply, KantanDB collects and sorts the candidate IDs before running jq.
It uses a full document scan if no plan applies or the collector exceeds its 10,000-entry budget.
The `endswith`, `contains`, and `test` scans can read more entries but still collect at most 10,000 unique IDs.

Index bounds use the stored value type and exact numeric values, not jq comparison rules.
For example, jq considers `"90000"` greater than `100000`, but the index excludes it from a numeric range.
Decimal rounding in jq can also cause the index to miss matches.
Indexed string tests skip missing and non-string fields, which can raise errors in a full document scan.
To force a full scan, wrap the entire program in parentheses: `(select(.salary > 100000) | .name)`.

## Stream completion

The server sends the first NDJSON line immediately and groups later lines into batches of up to 32 KiB. It sends larger lines on their own. NDJSON responses announce `Trailer: Query-Status`. Read the body to the end and check the trailer. `Query-Status: ok` confirms completion. `Query-Status: error` means the stream failed and can contain partial results. If a proxy or client drops the trailer, treat completion as unconfirmed. If the query fails before streaming starts, KantanDB returns an HTTP error with a JSON body.

## Limits

The program can contain up to 64 KiB of source, subject to the server request limit. JSON mode allows up to 10,000 input documents, 64 MiB of input, 8 MiB of output, and five seconds for the query. NDJSON has no total count or byte limit. It has a five-second deadline for each document evaluation and each write.

Restrict QUERY to trusted clients. Deadlines do not cap allocations. For example, `[., inputs]` collects all remaining documents in memory. KantanDB uses [gojq](https://github.com/itchyny/gojq#difference-to-jq) and does not expose local modules or environment variables. QUERY does not accept URI query parameters or return cursors.
