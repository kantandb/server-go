package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
)

type databaseInfo struct {
	Name          string         `json:"name"`
	Indices       []indexRequest `json:"indices"`
	DocumentCount uint64         `json:"doc_count"`
}

func (s *store) databaseInfo(ctx context.Context, database string) (info databaseInfo, infoErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	exists, err := s.hasDB(database)
	if err != nil {
		return info, fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return info, errDBNotFound
	}
	defs, err := s.indexes(database)
	if err != nil {
		return info, err
	}
	info.Name = database
	info.Indices = make([]indexRequest, 0, len(defs))
	for _, def := range defs {
		info.Indices = append(info.Indices, indexRequest{Name: def.name, Path: def.path})
	}

	// Count primary records without decrypting bodies. The iterator is a snapshot.
	prefix := docsPrefix(database)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return info, wrapStore("creating count iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			infoErr = errors.Join(infoErr, wrapStore("closing count iterator", err))
		}
	}()
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		id := string(iter.Key()[len(prefix):])
		if validateID(id) != nil {
			return info, fmt.Errorf("%w: invalid document ID", errCorruptData)
		}
		if _, _, _, err := parseDocRecord(iter.Value()); err != nil {
			return info, fmt.Errorf("%w: document %q", errCorruptData, id)
		}
		info.DocumentCount++
	}
	if err := iter.Error(); err != nil {
		return info, wrapStore("counting documents", err)
	}

	return info, nil
}
