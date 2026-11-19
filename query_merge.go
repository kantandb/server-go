package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
)

const maxMergeLists = 128

type indexList struct {
	iter   *pebble.Iterator
	keyLen int
	id     string
}

func scanMerged(ctx context.Context, snapshot *pebble.Snapshot, database string, databaseKey []byte, plan queryPlan, names map[string]string, scan func(func(context.Context) ([]byte, error)) error) (used bool, err error) {
	for _, part := range plan {
		if part.match != nil {
			return false, nil
		}
	}

	lists := make([]indexList, 0, len(plan))
	defer func() {
		for _, list := range lists {
			if closeErr := list.iter.Close(); closeErr != nil {
				err = errors.Join(err, wrapStore("closing query index list", closeErr))
			}
		}
	}()
	seen := make(map[string]struct{})
	for _, part := range plan {
		complete, err := findIndexLists(ctx, snapshot, database, names[part.path], part, func(key []byte) (bool, error) {
			if _, ok := seen[string(key)]; ok {
				return true, nil
			}
			if len(lists) == maxMergeLists {
				return false, nil
			}

			iter, err := snapshot.NewIter(&pebble.IterOptions{LowerBound: key, UpperBound: prefixEnd(key)})
			if err != nil {
				return false, wrapStore("opening query index list", err)
			}
			lists = append(lists, indexList{iter: iter, keyLen: len(key)})
			seen[string(key)] = struct{}{}

			return true, lists[len(lists)-1].setID(iter.First())
		})
		if err != nil || !complete {
			return false, err
		}
	}

	var last string
	return true, scan(func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, err := nextMergedID(lists, last)
		if err != nil || id == "" {
			return nil, err
		}
		last = id

		return readIndexedDoc(snapshot, databaseKey, docKey(database, id), id)
	})
}

func findIndexLists(ctx context.Context, snapshot *pebble.Snapshot, database, index string, part indexScan, add func([]byte) (bool, error)) (complete bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	bounds, prefix := scanBounds(database, index, part)
	if bytes.Compare(bounds.LowerBound, bounds.UpperBound) >= 0 {
		return true, nil
	}
	if part.op == cmpEq && !part.prefix {
		return add(prefix)
	}

	iter, err := snapshot.NewIter(&bounds)
	if err != nil {
		return false, wrapStore("finding query index lists", err)
	}
	defer func() {
		if closeErr := iter.Close(); closeErr != nil {
			err = errors.Join(err, wrapStore("closing query index iterator", closeErr))
		}
	}()

	// Seek past each value instead of walking its document IDs.
	for valid := iter.First(); valid; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		value, _, ok := splitIndexEntry(prefix, iter.Key())
		if !ok {
			return false, fmt.Errorf("%w: invalid index entry", errCorruptData)
		}
		key := indexValuePrefix(database, index, value)
		if complete, err := add(key); err != nil || !complete {
			return false, err
		}
		valid = iter.SeekGE(prefixEnd(key))
	}
	if err := iter.Error(); err != nil {
		return false, wrapStore("finding query index lists", err)
	}

	return true, nil
}

func (list *indexList) setID(valid bool) error {
	if !valid {
		list.id = ""
		if err := list.iter.Error(); err != nil {
			return wrapStore("reading query index list", err)
		}

		return nil
	}

	id := string(list.iter.Key()[list.keyLen:])
	if len(list.iter.Value()) != 0 || validateID(id) != nil {
		return fmt.Errorf("%w: invalid index entry", errCorruptData)
	}
	list.id = id

	return nil
}

func nextMergedID(lists []indexList, last string) (string, error) {
	var id string
	for i := range lists {
		list := &lists[i]
		// Advance every copy of the last ID to avoid emitting duplicates.
		if last != "" && list.id == last {
			if err := list.setID(list.iter.Next()); err != nil {
				return "", err
			}
		}
		if list.id != "" && (id == "" || list.id < id) {
			id = list.id
		}
	}

	return id, nil
}
