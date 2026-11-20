# jq queries

Send a raw UTF-8 jq program to `QUERY /db/{database}` with `Content-Type: text/plain`.

```sh
curl -i -N -X QUERY http://localhost:8080/db/users \
  -H 'Content-Type: text/plain' \
  -H 'Accept: application/x-ndjson' \
  --data-binary 'select(.age >= 30 and .age <= 40) | .name'
```

## Input and output

KantanDB runs the program on each document body, like `jq` without `-s`. It reads from one Pebble snapshot. Full scans read in document-ID order. Indexed queries read in index order and can return a different set of documents when you use `limit`. The input contains no server IDs or revisions. For example, `.` returns every document and `.skills[]` emits each skill separately. A program can use `input` or `inputs` to consume later documents from the same scan.

Each output has the shape `{"id": ID, "rev": ETAG, "doc": OUTPUT}`. The `id` and `rev` identify the source document. The revision is its ETag, including the quotes inside the JSON string. The `doc` field contains the full document or the jq result, including scalars, arrays, and `null`. Multiple outputs from one document share its metadata. If jq consumes `input` or `inputs`, outputs keep the metadata of the document that started that evaluation.

By default, KantanDB buffers these results in one JSON array. `Accept: application/x-ndjson` streams one compact result per line. An array from jq stays inside `doc` on one line. If jq emits no values, JSON returns `[]` and NDJSON returns an empty body. If `Accept` excludes both formats, KantanDB returns `406 Not Acceptable`.

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

A bare indexed filter such as `select(.category == 3)` returns the stored JSON without running jq.
Indexed equality compares number spellings exactly: `3`, `3.0`, and `3e0` match different values.
For other indexed queries, KantanDB runs the original jq program on documents as it finds them in the index. If a query uses several index scans, KantanDB runs them one at a time and skips documents it already read.

A query such as `limit(10; (., inputs) | (select(.category >= 3) | .name)), halt` can use the same index as its `select` filter.
The limit applies to outputs, and `halt` ends the scan after the limit.
KantanDB uses a full scan if no index plan applies. Other uses of `input` or `inputs` also use a full scan.

Range bounds use the stored value type and exact numeric values, regardless of number spelling.
For example, jq considers `"90000"` greater than `100000`, but the index excludes it from a numeric range.
Decimal rounding in jq can also cause the index to miss matches.
Indexed string tests skip missing and non-string fields, which can raise errors in a full document scan.
To force a full scan, wrap the entire program in parentheses: `(select(.salary > 100000) | .name)`.

## Pagination

Send `Limit: 100` to request a page of up to 100 results. Valid limits range from 1 to 1,000.
Requests without `Limit` or `Cursor` remain unpaginated. A request with only `Cursor` uses a limit of 100.
The limit counts outputs, not the documents that the filter examines.

For JSON, the response `Cursor` header contains the token for the next page.
For NDJSON, the response announces a `Cursor` trailer. Read the complete body and require `Query-Status: ok` before using that token.
If the cursor is absent after successful completion, there are no more results.
The server evaluates one extra matching result to detect another page, without returning that result.
A failed page returns no cursor.

Send the token in the `Cursor` request header with the exact same query text and database.
You can change `Limit` or the response format between pages.
Tokens are encrypted and authenticated and bind to the query, scan plan, index definitions, and database generation.
A changed query, incompatible index change, or database recreation invalidates the token. Restarting the server does not invalidate it.

Pagination accepts a conservative set of programs that produce at most one result per document:

- Identity, field access, literals, parentheses, and pipes.
- Comparisons, `and`, `or`, and `select` with supported expressions.
- Array and object construction, such as `[.name, .age]` and `{name: .name, age: .age}`.
- `startswith`, `endswith`, `contains`, `test`, `IN`, `length`, `type`, `not`, `ascii_downcase`, and `ascii_upcase` with supported arguments.

Programs such as `.skills[]`, `.name, .age`, `input`, `inputs`, and function definitions reject pagination.
Queries requiring multiple index scans, such as indexed OR or multi-value IN, also reject pagination.
Unsupported programs return `400` with `query_not_pageable`. Invalid limits or cursors return `invalid_limit` or `invalid_cursor`.
Unpaginated queries retain their existing behavior.

A cursor records a storage key, not a result offset. The next page seeks directly past that key.
Indexed pages preserve index order, including document IDs as tie-breakers for equal values.
Each request reads a new snapshot. Concurrent writes can cause omissions or repeated results, especially when indexed values change.
Existing evaluation limits still apply to each request. Sparse filters can require substantial scanning to fill a page.

## Stream completion

The server sends the first NDJSON line immediately and groups later lines into batches of up to 32 KiB. It sends larger lines on their own. NDJSON responses announce `Trailer: Query-Status`. Read the body to the end and check the trailer. `Query-Status: ok` confirms completion. `Query-Status: error` means the stream failed and can contain partial results. If a proxy or client drops the trailer, treat completion as unconfirmed. If the query fails before streaming starts, KantanDB returns an HTTP error with a JSON body.

## Limits

The program can contain up to 64 KiB of source, subject to the server request limit. JSON mode allows up to 10,000 input documents, 64 MiB of input, 8 MiB of output, and five seconds for the query. NDJSON has no total count or byte limit. It has a five-second deadline for each document evaluation and each write.

Restrict QUERY to trusted clients. Deadlines do not cap allocations. For example, `[., inputs]` collects all remaining documents in memory. KantanDB uses [gojq](https://github.com/itchyny/gojq#difference-to-jq) and does not expose local modules or environment variables. QUERY does not accept URI query parameters. Use headers for pagination.
