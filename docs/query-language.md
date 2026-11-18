# Query language

List documents with `GET /db/{database}`. Filter documents with `QUERY /db/{database}`. The response contains document IDs, not the document bodies:

```json
{
    "documents": ["01950000-0000-7000-8000-000000000001"],
    "cursor": ""
}
```

Fetch a returned document with `GET /db/{database}/{id}`.

## Query forms

These query forms are available:

```text
GET /db/users
    List every document ID in users.

QUERY /db/users
Content-Type: application/json

{"path":"$.profile.age","op":"ge","value":18}
    Find IDs through an RFC 9535 JSONPath.

QUERY /db/users
Content-Type: application/json

{"index":"email","value":"alice@example.com"}
    Find IDs through the named secondary index.
```

An indexed query has one condition:

```text
index <operator> JSON scalar
```

GET listing accepts only `limit` (1 to 1000, default 100) and `cursor` (the last document ID from the previous page). Unknown or repeated parameters are rejected. Filters belong in QUERY bodies.

A QUERY request has no URI parameters. Its JSON body has these fields:

| Field    | Meaning                                | Default  |
| -------- | -------------------------------------- | -------- |
| `index`  | Secondary-index name                   | none     |
| `path`   | RFC 9535 JSONPath                      | none     |
| `value`  | One JSON scalar to compare             | required |
| `op`     | `eq`, `lt`, `le`, `gt`, or `ge`        | `eq`     |
| `limit`  | Page size from 1 to 1000               | 100      |
| `cursor` | Position returned by the previous page | empty    |

Supply exactly one of `index` or `path`. Named-index queries use the index's JSON Pointer semantics, including array positions and numeric object properties. An unknown index returns `404 index_not_found`. Path queries keep automatic index selection and scan fallback.

The body must use `Content-Type: application/json`. Unknown or duplicate fields, trailing JSON, and URI parameters are rejected. QUERY responses use `Cache-Control: no-store`. Collection responses advertise the format with `Accept-Query: application/json`.

## Values and operators

`value` is a JSON scalar in the request body. Strings need JSON quotes:

```json
{"index":"email","value":"alice@example.com"}
```

With `curl`, send the JSON body with the QUERY method:

```sh
curl -X QUERY http://localhost:8080/db/users \
  -H 'Content-Type: application/json' \
  --data '{"index":"email","value":"alice@example.com"}'
```

Equality (`eq`) accepts strings, numbers, booleans, and `null`. Range operators work only with strings and numbers:

| Operator | Test                     |
| -------- | ------------------------ |
| `eq`     | equal to                 |
| `lt`     | less than                |
| `le`     | less than or equal to    |
| `gt`     | greater than             |
| `ge`     | greater than or equal to |

Numbers are compared exactly, without floating-point rounding. Equivalent JSON forms such as `1`, `1.0`, and `1e0` match each other. Strings use binary UTF-8 order. Types are not converted, so the string `"34"` does not match the number `34`.

A document is absent from an index when the indexed path is missing or contains an object or array. Such documents cannot match an indexed query.

JSONPath returns a list of nodes. A document matches a QUERY request when at least one selected scalar satisfies the comparison. Missing paths and selected objects or arrays do not match. Select their scalar children with JSONPath selectors such as `[*]` when needed.

## Examples

```text
QUERY /db/users
{"index":"active","value":true}
    Users whose indexed active field is true.

QUERY /db/users
{"index":"age","op":"ge","value":18}
    Users whose indexed age is at least 18.

QUERY /db/users
{"index":"name","op":"lt","value":"m"}
    Users whose indexed name sorts before "m".

GET /db/users?limit=25
    First 25 IDs without using a secondary index.

QUERY /db/users
{"path":"$.items[*].price","op":"lt","value":10,"limit":25}
    Users with any item priced below 10.
```

The language currently has no `AND`, `OR`, joins, projections, custom sorting, or full-text search.

## Ordering and pagination

```text
request with limit
        │
        ▼
 IDs plus one page cursor
        │
        ▼
same request with cursor
        │
        ▼
      next page
```

Unfiltered lists and equality queries are ordered by document ID. Range queries are ordered by indexed value, then document ID when values are equal.

A simple JSONPath is translated to JSON Pointer. If it matches a declared index, QUERY uses index order. Paths containing a numeric token scan because JSON Pointer cannot distinguish an array index from an object member name. Other JSONPaths also scan in document ID order. A scan page examines at most 10,000 documents, so it can return an empty page with a continuation cursor. Each request runs for at most five seconds.

If more results exist, `cursor` is non-empty. Send it back with the same query. The final page returns an empty cursor. A cursor does not identify a fixed snapshot across requests, so writes between pages can change later results.

For an unfiltered list, the cursor is the last document ID.

QUERY cursors are opaque and authenticated. They bind the method, database, index, normalized path when supplied, operator, value, chosen order, and scan position. Send one only with the same request body, apart from `limit`. A cursor stays valid after restart but becomes invalid if the database is deleted and recreated.

QUERY bodies are limited to 64 KiB. Paths are limited to 256 bytes, 16 levels of bracket or parenthesis nesting, and 32 top-level segments. To bound evaluation, paths may contain at most one descendant segment and one selector per segment. A document may contain at most 1,000,000 JSON nodes for scan evaluation.
