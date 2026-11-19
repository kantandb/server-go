package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/cockroachdb/pebble"
)

const (
	maxRangeProbeLimit = 128
	maxProbeBytes      = 1 << 20
)

type rangeProbe struct {
	field string
	bound int64
	op    cmpOp
	limit int
}

func planRangeProbe(part indexScan, limit int) *rangeProbe {
	// Leave strings, decimals, and compound ranges to the index planner.
	if part.op == cmpEq || part.upper != nil {
		return nil
	}
	bound, err := strconv.ParseInt(string(part.integer), 10, 64)
	if err != nil {
		return nil
	}
	field := strings.NewReplacer("~1", "/", "~0", "~").Replace(part.path[1:])

	return &rangeProbe{field: field, bound: bound, op: part.op, limit: limit}
}

func (p *rangeProbe) matches(doc []byte) (match, supported bool) {
	field, err := storedField(doc, p.field)
	if err != nil {
		return false, false
	}
	value, err := strconv.ParseInt(string(field), 10, 64)
	if err != nil {
		// The numeric index does not include missing or nonnumeric values.
		return false, field[0] != '-' && (field[0] < '0' || field[0] > '9')
	}
	switch p.op {
	case cmpLT:
		return value < p.bound, true
	case cmpLE:
		return value <= p.bound, true
	case cmpGT:
		return value > p.bound, true
	case cmpGE:
		return value >= p.bound, true
	default:
		return false, false
	}
}

// Try the primary key order first. Only use it if a short prefix satisfies the limit.
func scanDenseRange(ctx context.Context, snapshot *pebble.Snapshot, database string, key []byte, p *rangeProbe, scan func(func(context.Context) ([]byte, error)) error) (bool, error) {
	docs := make([][]byte, 0, p.limit)
	defer func() {
		for _, doc := range docs {
			clear(doc)
		}
	}()

	// Stop early on sparse data. Bound the extra reads and memory if the probe fails.
	remaining := maxProbeBytes
	err := scanDocSnapshot(snapshot, database, key, func(read func(context.Context) ([]byte, error)) error {
		for i := range p.limit + p.limit/3 {
			doc, err := read(ctx)
			if errors.Is(err, errCorruptData) {
				// Let the index path decide whether it needs this document.
				break
			}
			if err != nil || doc == nil {
				return err
			}
			match, supported := p.matches(doc)
			if !supported || len(doc) > remaining {
				clear(doc)
				return nil
			}
			remaining -= len(doc)
			if match {
				docs = append(docs, doc)
			} else {
				clear(doc)
			}
			if len(docs) == p.limit || (i == 7 && len(docs) < 6) {
				break
			}
		}

		return nil
	})
	if err != nil || len(docs) != p.limit {
		return false, err
	}

	i := 0
	return true, scan(func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i == len(docs) {
			return nil, nil
		}
		doc := docs[i]
		i++

		return doc, nil
	})
}
