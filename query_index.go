package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"
)

func scanIndexDocs(snapshot *pebble.Snapshot, database string, key []byte, plan queryPlan, names map[string]string, scan func(func(context.Context) ([]byte, error)) error) (scanErr error) {
	var iter *pebble.Iterator
	closeIter := func() error {
		if iter == nil {
			return nil
		}
		err := iter.Close()
		iter = nil
		if err != nil {
			return wrapStore("closing query index iterator", err)
		}

		return nil
	}
	defer func() {
		if err := closeIter(); err != nil {
			scanErr = errors.Join(scanErr, err)
		}
	}()

	// A single scan has one entry per document. Only unions need deduplication.
	var seen map[string]struct{}
	if len(plan) > 1 {
		seen = make(map[string]struct{})
	}
	var part indexScan
	var prefix []byte
	return scan(func(ctx context.Context) ([]byte, error) {
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var valid bool
			if iter == nil {
				if len(plan) == 0 {
					return nil, nil
				}
				part, plan = plan[0], plan[1:]
				bounds, keyPrefix := scanBounds(database, names[part.path], part)
				if bytes.Compare(bounds.LowerBound, bounds.UpperBound) >= 0 {
					continue
				}
				var err error
				iter, err = snapshot.NewIter(&bounds)
				if err != nil {
					return nil, wrapStore("opening query index iterator", err)
				}
				prefix = keyPrefix
				valid = iter.First()
			} else {
				valid = iter.Next()
			}
			if !valid {
				if err := iter.Error(); err != nil {
					return nil, wrapStore("reading query index iterator", err)
				}
				if err := closeIter(); err != nil {
					return nil, err
				}
				continue
			}

			id, err := indexMatch(ctx, iter, prefix, part)
			if err != nil {
				return nil, err
			}
			if id == "" {
				continue
			}
			if seen != nil {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
			}

			return readIndexedDoc(snapshot, key, docKey(database, id), id)
		}
	})
}

func indexMatch(ctx context.Context, iter *pebble.Iterator, prefix []byte, part indexScan) (string, error) {
	var encoded []byte
	var id string
	ok := true
	if part.op == cmpEq && !part.prefix && part.match == nil {
		id = string(iter.Key()[len(prefix):])
	} else {
		encoded, id, ok = splitIndexEntry(prefix, iter.Key())
	}
	if !ok || len(iter.Value()) != 0 || validateID(id) != nil {
		return "", fmt.Errorf("%w: invalid index entry", errCorruptData)
	}
	if part.match != nil {
		value := strings.ReplaceAll(string(encoded[1:len(encoded)-2]), "\x00\xff", "\x00")
		matched, valid := part.match(ctx, value)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !valid {
			return "", fmt.Errorf("%w: evaluating indexed string filter", errQueryRuntime)
		}
		if !matched {
			return "", nil
		}
	}

	return id, nil
}
