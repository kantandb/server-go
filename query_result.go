package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
)

const maxQueryBytes = 8 << 20

type queryDoc struct {
	ID  string          `json:"id"`
	Rev string          `json:"rev"`
	Doc json.RawMessage `json:"doc"`
}

type resultPage struct {
	docs      []queryDoc
	lastID    string
	lastValue []byte
	more      bool
}

func (s *store) executePathQuery(ctx context.Context, database string, plan pathPlan, query pathQuery) (docs []queryDoc, token string, queryErr error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	// Keep the database key and all returned records in one database lifetime.
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	key, err := s.databaseKey(database)
	if err != nil {
		return nil, "", err
	}
	defer clear(key)

	if plan.index != "" {
		defs, err := s.indexes(database)
		if err != nil {
			return nil, "", err
		}
		def, ok := findIndex(defs, plan.index)
		if !ok {
			return nil, "", errIndexNotFound
		}
		// A database can be recreated between planning and execution.
		if plan.path != nil {
			pointer, compatible := jsonPathPointer(plan.path)
			if !compatible || def.path != pointer {
				plan.index = ""
			}
		}
	}

	cursorKey, err := deriveCursorKey(key)
	if err != nil {
		return nil, "", err
	}
	defer clear(cursorKey)
	box, err := newQueryCipher(cursorKey)
	if err != nil {
		return nil, "", err
	}
	encoded, err := encodeIndexValue(query.value)
	if err != nil {
		return nil, "", err
	}

	var after pathCursor
	if query.cursor != "" {
		after, err = decodePathCursor(box, query.cursor)
		if err != nil || matchPathCursor(after, database, plan, query, encoded) != nil {
			return nil, "", errInvalidQueryCursor
		}
	}

	// Match and hydrate against the same snapshot so bodies cannot drift.
	snapshot := s.db.NewSnapshot()
	defer func() {
		if err := snapshot.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing result snapshot", err))
		}
	}()
	page, err := queryResultPage(ctx, snapshot, key, database, plan, query, encoded, after, maxQueryBytes, maxScanDocs)
	if err != nil || !page.more {
		return page.docs, "", err
	}

	order := orderDocument
	if plan.index != "" {
		order = orderIndex
	}
	token, err = encodePathCursor(box, pathCursor{
		method: queryMethod, dialect: dialectJSONPath, order: order,
		database: database, pathHash: pathHash(plan.text), index: plan.index,
		op: query.op, value: encoded, lastValue: page.lastValue, id: page.lastID,
	})

	return page.docs, token, err
}

func resultBounds(database string, plan pathPlan, query pathQuery, encoded []byte) (pebble.IterOptions, []byte) {
	prefix := docsPrefix(database)
	if plan.index != "" {
		prefix = indexValuePrefix(database, plan.index, encoded)
		if query.op != cmpEq {
			prefix = indexTypePrefix(database, plan.index, encoded[0])
		}
	}
	options := pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)}
	if plan.index == "" || query.op == cmpEq {
		return options, prefix
	}

	boundary := indexValuePrefix(database, plan.index, encoded)
	switch query.op {
	case cmpLT:
		options.UpperBound = boundary
	case cmpLE:
		options.UpperBound = prefixEnd(boundary)
	case cmpGT:
		options.LowerBound = prefixEnd(boundary)
	case cmpGE:
		options.LowerBound = boundary
	}

	return options, prefix
}

func queryResultPage(ctx context.Context, snapshot *pebble.Snapshot, key []byte, database string, plan pathPlan, query pathQuery, encoded []byte, after pathCursor, maxBytes, maxWork int) (page resultPage, queryErr error) {
	options, prefix := resultBounds(database, plan, query, encoded)
	iter, err := snapshot.NewIter(&options)
	if err != nil {
		return page, wrapStore("creating result iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing result iterator", err))
		}
	}()

	valid := iter.First()
	if after.id != "" {
		start := docKey(database, after.id)
		if plan.index != "" {
			value := encoded
			if query.op != cmpEq {
				value = after.lastValue
			}
			start = indexKey(database, plan.index, value, after.id)
		}
		valid = iter.SeekGE(start)
		if valid && bytes.Equal(iter.Key(), start) {
			valid = iter.Next()
		}
	}

	// Bound scan work and page bytes. Always allow one record to make progress.
	used := 0
	for work := 0; valid; valid, work = iter.Next(), work+1 {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		if plan.index == "" && work >= maxWork {
			page.more = true
			break
		}

		id := string(iter.Key()[len(prefix):])
		var value []byte
		if plan.index != "" {
			if query.op != cmpEq {
				var ok bool
				value, id, ok = splitIndexEntry(prefix, iter.Key())
				if !ok {
					return page, fmt.Errorf("%w: invalid index entry", errCorruptData)
				}
			}
			if len(iter.Value()) != 0 {
				return page, fmt.Errorf("%w: invalid index entry", errCorruptData)
			}
		}
		if validateID(id) != nil {
			return page, fmt.Errorf("%w: invalid document ID", errCorruptData)
		}

		doc, err := resultDocument(snapshot, key, database, id)
		if err != nil {
			return page, err
		}
		matches := true
		if plan.path != nil && plan.index == "" {
			var root any
			decoder := json.NewDecoder(bytes.NewReader(doc.json))
			decoder.UseNumber()
			if err := decoder.Decode(&root); err != nil {
				return page, fmt.Errorf("%w: invalid document JSON", errCorruptData)
			}
			matches, err = pathMatches(ctx, plan.path, root, query.op, encoded, maxQueryNodes)
			if err != nil {
				return page, err
			}
		}
		if matches {
			record := queryDoc{ID: id, Rev: hex.EncodeToString(doc.revision[:]), Doc: doc.json}
			body, err := json.Marshal(record)
			if err != nil {
				return page, fmt.Errorf("encoding result document: %w", err)
			}
			if len(page.docs) >= query.limit || len(page.docs) > 0 && used+len(body) > maxBytes {
				page.more = true
				break
			}
			page.docs = append(page.docs, record)
			used += len(body)
		}
		page.lastID = id
		page.lastValue = bytes.Clone(value)
	}
	if err := iter.Error(); err != nil {
		return page, wrapStore("iterating results", err)
	}

	return page, nil
}

func resultDocument(snapshot *pebble.Snapshot, key []byte, database, id string) (doc storedDoc, readErr error) {
	pebbleKey := docKey(database, id)
	value, closer, err := snapshot.Get(pebbleKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return doc, fmt.Errorf("%w: query references missing document", errCorruptData)
	}
	if err != nil {
		return doc, wrapStore("reading result document", err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			readErr = errors.Join(readErr, wrapStore("closing result document", err))
		}
	}()

	return openDoc(pebbleKey, key, id, value)
}
