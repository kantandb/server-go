# KantanDB server

KantanDB is an encrypted JSON document database with secondary indexes and
JSONPath queries over HTTP.

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
JSONPath queries use the HTTP `QUERY` method, so OpenAPI tooling must support
version 3.2.

### Usage examples

```sh
# Show the service name and build version.
xh GET localhost:8080/

# Check health.
xh GET localhost:8080/healthz

# Create a database with email and age indexes.
xh POST localhost:8080/db name=example \
  indexes:='[{"name":"email","path":"/email"},{"name":"age","path":"/age"}]'

# List databases, optionally after a cursor.
xh GET localhost:8080/db limit==100 cursor==example

# Create a document.
xh POST localhost:8080/db/example name=KantanDB \
  email=alice@example.com age:=34 active:=true

# List document IDs, optionally after a cursor.
xh GET localhost:8080/db/example limit==100 cursor==01950000-0000-7000-8000-000000000001

# Query the email index. value is a JSON string; op defaults to eq.
xh GET localhost:8080/db/example index==email value=='"alice@example.com"' limit==100

# Find documents whose indexed age is at least 30.
xh GET localhost:8080/db/example index==age op==ge value==30 limit==100

# Query any JSONPath without putting it in the URI.
echo '{"path":"$.age","op":"ge","value":30,"limit":100}' | \
  xh QUERY localhost:8080/db/example Content-Type:application/json

# Continue a JSONPath query with its opaque cursor.
echo '{"path":"$.age","op":"ge","value":30,"cursor":"eyJ..."}' | \
  xh QUERY localhost:8080/db/example Content-Type:application/json

# Continue an index query with its opaque cursor.
xh GET localhost:8080/db/example index==age op==ge value==30 cursor==eyJ...

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

# Export documents as NDJSON.
xh GET localhost:8080/bulk/example --download --output example.ndjson

# Import them with new IDs and revisions.
xh POST localhost:8080/bulk/target \
  Content-Type:application/x-ndjson < example.ndjson

# Delete a database.
xh DELETE localhost:8080/db/example
```

### Index queries

Index queries support `eq`, `lt`, `le`, `gt`, and `ge`; the default is `eq`.
Equality works with any JSON scalar. Ordering works with numbers and strings.
Types are not coerced, and missing paths, objects, arrays, or values of another
type do not match. Numbers are compared exactly. Strings use binary UTF-8
order.

Range results are ordered by indexed value, then document ID. Equality results
are ordered by document ID. Each query reads one page plus at most one extra
valid index entry. To fetch the next page, reuse the returned cursor with the
same database, index, operator, and value.

### JSONPath queries

Send an RFC 9535 JSONPath query in the body of `QUERY /db/{database}`. A document
matches when any selected scalar satisfies the comparison.

A simple path uses its declared index unless it contains a numeric token. Other
paths scan documents in ID order. A scan examines no more than 10,000 documents
per page and stops after five seconds.

The request body is limited to 64 KiB and the path to 256 bytes. A path may have
one descendant and one selector per segment. Evaluation visits at most 1,000,000
JSON nodes per document.

### Bulk transfer

`GET /bulk/{database}` streams canonical documents as NDJSON from one Pebble
snapshot. `POST /bulk/{database}` accepts that stream, assigns new UUIDv7 IDs
and revisions, applies the target database's indexes, and commits it atomically.

Defaults allow 1 MiB per document, 64 MiB and 1,000 documents per import, a
128 MiB Pebble batch, 30 seconds, and one bulk request at a time. Configure them
with the `-bulk-*` flags shown by `kantan -help`.

Bulk transfer preserves document values only. It omits database and index
definitions, IDs, revisions, ETags, and encryption keys, so it is not backup and
restore.
