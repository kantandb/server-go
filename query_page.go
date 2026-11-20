package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cockroachdb/pebble"
	"github.com/itchyny/gojq"
)

var (
	errQueryCursor = errors.New("invalid query cursor")
	errPageLimit   = errors.New("invalid query page limit")
	errQueryPage   = errors.New("query does not support pagination")
	errPageDone    = errors.New("query page complete")
)

type queryPage struct {
	limit   int
	cursor  string
	after   []byte
	current []byte
	last    []byte
	count   int
	next    string
}

func parseQueryPage(header http.Header) (*queryPage, error) {
	limits, cursors := header.Values("Limit"), header.Values("Cursor")
	if len(limits) == 0 && len(cursors) == 0 {
		return nil, nil
	}
	page := &queryPage{limit: defaultListLimit}
	if len(limits) != 0 {
		if len(limits) != 1 {
			return nil, errPageLimit
		}
		limit, err := strconv.Atoi(limits[0])
		if err != nil || limit < 1 || limit > maxListLimit {
			return nil, errPageLimit
		}
		page.limit = limit
	}
	if len(cursors) != 0 {
		if len(cursors) != 1 || cursors[0] == "" || len(cursors[0]) > maxQueryCursor {
			return nil, errQueryCursor
		}
		page.cursor = cursors[0]
	}

	return page, nil
}

// Resume strictly after the saved key, even if that entry was deleted.
func pageSeek(iter *pebble.Iterator, page *queryPage) bool {
	if page == nil || len(page.after) == 0 {
		return iter.First()
	}
	valid := iter.SeekGE(page.after)
	if valid && bytes.Equal(iter.Key(), page.after) {
		return iter.Next()
	}

	return valid
}

func (page *queryPage) emit(value []byte, emit func([]byte) error) error {
	if page.count == page.limit {
		return errPageDone
	}
	if err := emit(value); err != nil {
		return err
	}
	page.count++
	page.last = append(page.last[:0], page.current...)

	return nil
}

func (s *store) runPage(ctx context.Context, database string, program []byte, query *gojq.Query, stream bool, page *queryPage, emit func([]byte) error) error {
	if !pageQuery(query, 128) {
		return errQueryPage
	}
	plan := planQuery(query)
	code, err := gojq.Compile(query)
	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidQuery, err)
	}

	return s.withDocSnapshot(database, func(snapshot *pebble.Snapshot, key []byte) error {
		defs, err := s.indexes(database)
		if err != nil {
			return err
		}
		names := planIndexNames(plan, defs)
		if names == nil {
			plan = nil
		}
		if len(plan) > 1 {
			return errQueryPage
		}
		index := ""
		if len(plan) == 1 {
			index = names[plan[0].path]
		}
		binding, err := pageBinding(database, program, index, defs)
		if err != nil {
			return err
		}
		box, err := pageCipher(key)
		if err != nil {
			return err
		}
		if page.cursor != "" {
			page.after, err = decodeQueryCursor(box, binding, page.cursor)
			if err != nil {
				return err
			}
		}

		output := func(value []byte) error { return page.emit(value, emit) }
		direct := planDirect(query)
		if selectCall := queryCall(query, "select", 1); selectCall != nil &&
			ungroup(selectCall.Args[0]).Op == gojq.OpEq && len(plan) == 1 {
			direct = &directPlan{}
		}
		scan := directScan(direct, stream, ctx, output)
		if direct == nil {
			scan = func(read func(context.Context) (*queryDocument, error)) error {
				input := &queryInput{read: read, limit: !stream}
				for {
					done, err := evalQueryDoc(ctx, input, code, stream, output)
					if done || err != nil {
						return err
					}
				}
			}
		}
		if len(plan) == 1 {
			err = scanIndexDocs(snapshot, database, key, plan, names, page, scan)
		} else {
			err = scanDocSnapshot(snapshot, database, key, page, scan)
		}
		if err == errPageDone {
			page.next, err = encodeQueryCursor(box, binding, page.last)
		}

		return err
	})
}
