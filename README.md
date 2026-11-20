# KantanDB server

KantanDB is an encrypted JSON document database with secondary indexes and
jq queries over HTTP.

## Start the server

Create a master key outside the data directory, then build and start KantanDB:

```sh
openssl rand -base64 32 > kantan.key
chmod 600 kantan.key
mise run build
./kantan -addr :8080 -data data -key-file kantan.key -max-body-bytes 1048576
```

The server requires a base64-encoded 32-byte key. Keep it safe: losing it makes
the data unreadable. The server also refuses to start with the wrong key or an
older plaintext store.

## HTTP API

The examples below use [`xh`](https://github.com/ducaale/xh). See the
[OpenAPI contract](openapi.yaml) for complete request and response schemas.
jq queries use the HTTP `QUERY` method, so OpenAPI tooling must support
version 3.2.

### Usage examples

```sh
# Show the service name and build version.
xh GET localhost:8080/

# Check health.
xh GET localhost:8080/healthz

# Create a database with email and age indexes.
xh POST localhost:8080/db name=example \
  indices:='[{"name":"email","path":"/email"},{"name":"age","path":"/age"}]'

# List databases, optionally after a cursor. The response Cursor identifies the next page.
xh GET localhost:8080/db Limit:100 Cursor:example

# Create a document.
xh POST localhost:8080/db/example name=KantanDB \
  email=alice@example.com age:=34 active:=true

# Read the database name, index definitions, and document count.
xh GET localhost:8080/db/example

# List document bodies.
printf '%s\n' '.' | \
  xh QUERY localhost:8080/db/example Content-Type:text/plain

# Return names of documents whose age is at least 30.
printf '%s\n' 'select(.age >= 30) | .name' | \
  xh QUERY localhost:8080/db/example Content-Type:text/plain

# Read a document using the returned ID.
xh GET localhost:8080/db/example/01950000-0000-7000-8000-000000000001

# Replace a document if its ETag still matches.
xh PUT localhost:8080/db/example/01950000-0000-7000-8000-000000000001 \
  If-Match:'"0123456789abcdef0123456789abcdef"' name=Updated

# Merge fields into a document.
xh PATCH localhost:8080/db/example/01950000-0000-7000-8000-000000000001 \
  Content-Type:application/merge-patch+json active:=false

# Apply a JSON Patch.
echo '[{"op":"replace","path":"/name","value":"Patched"}]' | \
  xh PATCH localhost:8080/db/example/01950000-0000-7000-8000-000000000001 \
  Content-Type:application/json-patch+json

# Delete a document if it exists.
xh DELETE localhost:8080/db/example/01950000-0000-7000-8000-000000000001 If-Match:\*

# Export documents as NDJSON. Check the Query-Status trailer before using the file.
curl -sS --fail-with-body -X QUERY http://localhost:8080/db/example \
  -H 'Content-Type: text/plain' -H 'Accept: application/x-ndjson' \
  --data-binary '.' -D export.headers -o example.ndjson

# Import them with new IDs and revisions.
xh POST localhost:8080/db/target \
  Content-Type:application/x-ndjson < example.ndjson

# Delete a database.
xh DELETE localhost:8080/db/example
```

### jq queries

Send a raw jq program as `text/plain`. KantanDB runs it on each document.
Each result is `{"id": ID, "rev": ETAG, "doc": OUTPUT}`. The revision matches
the source document's ETag. By default, results form a JSON array. Set
`Accept: application/x-ndjson` to stream one result per line. A final
`Query-Status: ok` trailer confirms the stream finished.

Set `Limit: 100` to paginate supported single-output queries, up to 1,000 results per page.
Send the returned `Cursor` with the same query to request the next page.
JSON returns the cursor in a response header. NDJSON returns it in a trailer.
Requests without `Limit` or `Cursor` remain unpaginated.
Restrict access to trusted clients. See the [query guide](docs/query-language.md)
for limits and examples.

### Bulk transfer

`QUERY` with `.` exports documents as NDJSON when you request that format.
Send `Content-Type: application/x-ndjson` to `POST /db/{database}` to import a batch.
Each line accepts a raw document or an exact wrapper with string `id` and `rev` fields and an object `doc`.
Imports discard wrapper IDs and revisions and store only `doc`.
KantanDB assigns new UUIDv7 IDs and revisions and updates the documents and indexes in one commit.
The batch returns `200 OK`, even for one line. A JSON object sent as
`application/json` still returns `201 Created`.

Defaults allow 1 MiB per document, 64 MiB and 1,000 documents per import, a
128 MiB Pebble batch, 30 seconds, and one bulk request at a time. Configure them
with the `-bulk-*` flags shown by `kantan -help`.

Bulk transfer preserves document values only. It does not restore IDs or revisions
or include database and index definitions or encryption keys, so it is not backup and
restore.
