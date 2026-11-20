# Storage model

KantanDB keeps all logical databases in one [Pebble](https://github.com/cockroachdb/pebble) key-value store. The `-data` option selects its directory (`data` by default). Pebble owns the files in that directory, including its write log, manifests, and sorted-table (`.sst`) files. They should not be edited directly.

A KantanDB "database" is therefore a named collection inside one Pebble store, not a separate Pebble database or directory.

```text
-data directory
└── one Pebble store
    ├── encrypted store metadata
    ├── wrapped database keys
    ├── encrypted document records
    ├── secondary-index definitions
    └── plaintext secondary-index entries
```

## Encryption boundary

Document JSON is compressed and encrypted before it reaches Pebble. This protects document bodies in WAL and SST files when the master-key file is stored separately.

Encryption does not hide database names, index names, document IDs, indexed values, record sizes, or plaintext left in old stores and backups. Pebble needs plaintext secondary-index values for ordered scans.

The `-key-file` must contain one base64-encoded 32-byte key. KantanDB derives a store wrapping key from it and a random 32-byte store salt. Each database has a random 32-byte key stored only as AES-256-GCM ciphertext. HKDF-SHA256 derives per-document keys from the database key.

Deleting and recreating a database creates a new database key. Its old document records cannot be decrypted with the new key.

## Logical key layout

The first byte of every key says what kind of record follows. Names and IDs are included in the key, so Pebble's byte ordering groups related records together.

```text
0x00
    Store metadata: version | key ID | salt | nonce | encrypted verifier

0x01 | database
    Database record: version | key ID | nonce | encrypted database key

0x02 | database | 0x00 | document ID
    Document record / primary index entry

0x03 | len(database) | database | len(index) | index
    Secondary-index definition

0x04 | len(database) | database | len(index) | index | encoded value | document ID
    Secondary-index entry
```

Lengths in keys are unsigned variable-length integers. The encrypted formats use fixed-width headers. Version 1 supports wrapping-key ID 1 and cipher suite 1 only.

## Documents and the primary index

A document's key contains its database name and ID. Its value is:

```text
version (1) | suite (1) | revision (16) | original length (8) |
nonce (12) | encrypted Zstandard data | GCM tag (16)
```

Suite 1 uses Zstandard, HKDF-SHA256, and AES-256-GCM. The exact Pebble key and visible header are authenticated as associated data. Changing the database name, ID, revision, size, nonce, ciphertext, or tag makes the record unreadable.

KantanDB authenticates the header before trusting the original length. Decompression has a 64 MiB output limit and must produce exactly the authenticated length. Counting documents for database information checks only the fixed envelope shape; it does not decrypt each body.

This key space is the primary index. There is no separate ID lookup table. Scanning `0x02 | database | 0x00` lists documents in byte order. UUIDv7 IDs normally sort by creation time. The random revision is the document ETag.

## Secondary indexes

A secondary index is declared when its database is created. Its definition stores the index name and a JSON Pointer such as `/email` or `/items/0/sku`.

For each document, KantanDB follows that pointer and writes an empty Pebble value under a key containing:

```text
database → index name → encoded field value → document ID
```

The document ID at the end handles duplicate values and points back to the primary record. For example, two users with the same indexed email produce two adjacent keys:

```text
users → email → "a@example.com" → 0195...001
users → email → "a@example.com" → 0195...002
```

Index values have type tags. `null`, booleans, numbers, and strings are supported. Numbers and strings use order-preserving encodings, which lets Pebble prefix and range scans answer equality and range queries. Missing fields, objects, and arrays do not get an index entry. Types are kept separate and are not coerced.

Numeric keys store the sortable value followed by the original number spelling. Equality uses both parts, so `3` and `3.0` have different keys. Range bounds use only the sortable value.

A query scans the matching index keys and reads their document IDs. Equality matches are ordered by document ID. Range results are ordered by encoded value and then document ID.

## jq queries

`QUERY` reads one Pebble snapshot. A full scan reads in document-ID order. An
indexed query reads candidate documents in index order, without sorting them
by ID. See [jq queries](query-language.md#secondary-indexes)
for supported filters and comparison limits. Queries do not use cursors.

## Secondary-index range scans

Numbers and strings are encoded so their byte order matches their value order. All values of the same type also share a key prefix:

```text
index prefix → type → encoded value → document ID
               └──────── scan range ─────────┘
```

The storage layer sets Pebble iterator bounds around the requested value:

| Operator | Scanned keys                            |
| -------- | --------------------------------------- |
| `lt`     | Before the value                        |
| `le`     | Before or starting with the value       |
| `gt`     | After every key starting with the value |
| `ge`     | Starting with or after the value        |

The type prefix limits the scan to either numbers or strings. A numeric range cannot include strings, and a string range cannot include numbers. Booleans and `null` support equality only.

Internal range scans use a Pebble snapshot. The iterator moves forward in encoded-value order. The last encoded value and document ID identify the next scan position.

## Writes and deletion

Document creation, replacement, patching, and deletion update the primary record and all affected secondary entries in one synced Pebble batch. Readers cannot see a document change without its matching index change.

Deleting a logical database removes its database record and the ranges containing its documents, index definitions, and index entries in one batch.

## Format compatibility

Storage formats are unstable during the prototype phase. KantanDB does not read or migrate plaintext records. A directory with data but no valid encrypted-store metadata fails to open. Upgrades may require deleting the data directory.
