# Query language

Use `QUERY /db/{database}` to list or filter documents. Use `GET /db/{database}` to read database information.

## Database information

`GET /db/users` returns the database name, index definitions, and exact document count:

```json
{
    "name": "users",
    "indices": [{"name": "email", "path": "/email"}],
    "doc_count": 42
}
```

An empty database returns `indices: []` and `doc_count: 0`. The count scans primary records without decrypting bodies. GET accepts no URI query parameters.

## Query forms

Send a JSON object with `Content-Type: application/json`. An empty object lists every document in ID order:

```http
QUERY /db/users
Content-Type: application/json

{}
```

Add `limit` or `cursor` to page through an unfiltered list:

```json
{"limit":25,"cursor":"opaque-token-from-previous-page"}
```

Supply `index` to query a named secondary index:

```json
{"index":"email","value":"alice@example.com"}
```

Supply `path` to query an RFC 9535 JSONPath:

```json
{"path":"$.profile.age","op":"ge","value":18}
```

For a filter, supply exactly one of `index` or `path`, plus `value`. Without either selector, omit `op` and `value`. Unknown or duplicate fields, trailing JSON, and URI query parameters are rejected. An empty HTTP body is invalid; send `{}` instead.

| Field    | Meaning                                | Default |
| -------- | -------------------------------------- | ------- |
| `index`  | Secondary-index name                   | none    |
| `path`   | RFC 9535 JSONPath                      | none    |
| `value`  | One JSON scalar to compare             | none    |
| `op`     | `eq`, `lt`, `le`, `gt`, or `ge`         | `eq`    |
| `limit`  | Maximum page size from 1 to 1000        | 100     |
| `cursor` | Position returned by the previous page | empty   |

Named-index queries use the index's JSON Pointer semantics, including array positions and numeric object properties. An unknown index returns `404 index_not_found`. Path queries select a compatible index automatically or scan documents.

With `curl`, send the JSON body with the QUERY method:

```sh
curl -X QUERY http://localhost:8080/db/users \
  -H 'Content-Type: application/json' \
  --data '{"index":"email","value":"alice@example.com"}'
```

## Results

Every query returns records with server metadata outside the stored document:

```json
{
    "documents": [
        {
            "id": "01950000-0000-7000-8000-000000000001",
            "rev": "0123456789abcdef0123456789abcdef",
            "doc": {"name":"Alice","age":34}
        }
    ],
    "cursor": ""
}
```

`rev` is the document's ETag value without quotes. Add double quotes when sending it in `If-Match`. Application fields named `id` or `rev` stay inside `doc` and do not replace server metadata. An empty result returns `documents: []` and `cursor: ""`.

Each page selects matches and reads document bodies from one storage snapshot. QUERY responses use `Cache-Control: no-store`. Collection responses advertise the format with `Accept-Query: application/json`.

## Values and operators

`value` is a JSON scalar. Equality accepts strings, numbers, booleans, and `null`. Range operators accept only strings and numbers:

| Operator | Test                     |
| -------- | ------------------------ |
| `eq`     | equal to                 |
| `lt`     | less than                |
| `le`     | less than or equal to    |
| `gt`     | greater than             |
| `ge`     | greater than or equal to |

Numbers are compared exactly, without floating-point rounding. Equivalent JSON forms such as `1`, `1.0`, and `1e0` match each other. Strings use binary UTF-8 order. Types are not converted: the string `"34"` does not match the number `34`.

An indexed path with a missing field, object, or array has no index entry. Such documents cannot match an indexed query.

For a JSONPath filter, a document matches when any selected scalar satisfies the comparison. Missing paths, selected objects, and selected arrays do not match. Select their scalar children with selectors such as `[*]`.

The language has no `AND`, `OR`, joins, projections, custom sorting, or full-text search.

## Ordering and pagination

Unfiltered queries and equality queries use document-ID order. Indexed range queries use indexed-value order, then document-ID order for equal values.

A simple JSONPath translates to a JSON Pointer. If that pointer matches a declared index, QUERY uses the index. Numeric path segments prevent automatic selection because JSON Pointer cannot distinguish array positions from numeric object properties. Other paths also scan documents in ID order.

Each scan page examines at most 10,000 documents. A scan can return no matches and still supply a continuation cursor. Each QUERY request has a five-second execution limit.

Pages have an 8 MiB budget for serialized records, apart from the response envelope and separators. A page always permits one record, even if that record exceeds the budget. The requested `limit` is a maximum, not a guaranteed page size.

A non-empty `cursor` means that another page exists. Send it with the same query; only `limit` can change. The final page returns an empty cursor. Cursors are opaque, authenticated tokens bound to the database, filter, selected execution order, and scan position. Do not inspect or modify them.

A cursor survives server restarts but becomes invalid if its database is deleted and recreated. It does not preserve a snapshot between requests. Writes between pages can change later results.

QUERY bodies are limited to 64 KiB. Paths are limited to 256 bytes, 16 levels of bracket or parenthesis nesting, and 32 top-level segments. Paths can contain at most one descendant segment and one selector per segment. Scan evaluation visits at most 1,000,000 JSON nodes per document.
