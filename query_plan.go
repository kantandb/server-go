package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"
	"github.com/itchyny/gojq"
)

const maxPlanScans = 32

type queryPlan []indexScan

type indexScan struct {
	path    string
	op      cmpOp
	encoded []byte
	upperOp cmpOp
	upper   []byte
	prefix  bool
	match   func(context.Context, string) (bool, bool)
}

func planQuery(query *gojq.Query) queryPlan {
	if !plainQuery(query) {
		return nil
	}
	if query.Op == gojq.OpComma && queryCall(query.Right, "halt", 0) != nil {
		return planLimitedQuery(query.Left)
	}

	return planSelect(query)
}

// This form filters the entire input stream once, then ends the query.
func planLimitedQuery(q *gojq.Query) queryPlan {
	limit := queryCall(q, "limit", 2)
	if limit == nil {
		return nil
	}
	generator := ungroup(limit.Args[1])
	if !plainQuery(generator) || generator.Op != gojq.OpPipe {
		return nil
	}
	input := ungroup(generator.Left)
	if !plainQuery(input) || input.Op != gojq.OpComma || queryCall(input.Right, "inputs", 0) == nil {
		return nil
	}
	first := simpleTerm(input.Left)
	if first == nil || first.Type != gojq.TermTypeIdentity {
		return nil
	}
	count := simpleTerm(limit.Args[0])
	if count == nil || count.Type != gojq.TermTypeNumber || strings.Trim(count.Number, "0123456789") != "" {
		return nil
	}

	return planSelect(ungroup(generator.Right))
}

func queryCall(q *gojq.Query, name string, argc int) *gojq.Func {
	term := simpleTerm(q)
	if term == nil || term.Type != gojq.TermTypeFunc || term.Func.Name != name || len(term.Func.Args) != argc {
		return nil
	}

	return term.Func
}

func ungroup(q *gojq.Query) *gojq.Query {
	for term := simpleTerm(q); term != nil && term.Type == gojq.TermTypeQuery; term = simpleTerm(q) {
		q = term.Query
	}

	return q
}

// Only a leading select and optional field projection can use an index.
func planSelect(query *gojq.Query) queryPlan {
	if !plainQuery(query) {
		return nil
	}
	if query.Op == gojq.OpPipe {
		projection := simpleTerm(query.Right)
		if projection == nil || projection.Type != gojq.TermTypeIdentity && !isField(projection) {
			return nil
		}
		query = query.Left
	}
	term := simpleTerm(query)
	if term == nil || term.Type != gojq.TermTypeFunc || term.Func.Name != "select" || len(term.Func.Args) != 1 {
		return nil
	}

	return planPredicate(term.Func.Args[0], maxPlanScans)
}

func planPredicate(q *gojq.Query, budget int) queryPlan {
	if budget == 0 || !plainQuery(q) {
		return nil
	}
	switch q.Op {
	case gojq.OpOr:
		return planUnion(q, budget, planPredicate)
	case gojq.OpAnd:
		return planBetween(q.Left, q.Right)
	case gojq.OpPipe:
		return planFieldCall(q.Left, q.Right, budget)
	default:
		if term := simpleTerm(q); term != nil && term.Type == gojq.TermTypeQuery {
			return planPredicate(term.Query, budget-1)
		}
		if scan, ok := planComparison(q); ok {
			return queryPlan{scan}
		}

		return nil
	}
}

// Every branch must have a plan. Reserve space for at least one right scan.
func planUnion(q *gojq.Query, budget int, plan func(*gojq.Query, int) queryPlan) queryPlan {
	left := plan(q.Left, budget-1)
	if len(left) == 0 {
		return nil
	}
	right := plan(q.Right, budget-len(left))
	if len(right) == 0 {
		return nil
	}

	return append(left, right...)
}

func planComparison(q *gojq.Query) (indexScan, bool) {
	if !plainQuery(q) {
		return indexScan{}, false
	}
	op, ok := indexComparison(q.Op)
	if !ok {
		return indexScan{}, false
	}

	return planLiteral(fieldPath(q.Left), op, q.Right)
}

func planLiteral(path string, op cmpOp, q *gojq.Query) (indexScan, bool) {
	term := simpleTerm(q)
	if path == "" || term == nil {
		return indexScan{}, false
	}
	value := indexLiteral(term)
	if value == nil {
		return indexScan{}, false
	}
	encoded, err := encodeIndexValue(value)
	if err != nil || op != cmpEq && encoded[0] != 0x03 && encoded[0] != 0x04 {
		return indexScan{}, false
	}

	return indexScan{path: path, op: op, encoded: encoded}, true
}

func planBetween(left, right *gojq.Query) queryPlan {
	lower, lowerOK := planComparison(left)
	upper, upperOK := planComparison(right)
	if !lowerOK || !upperOK || lower.path != upper.path || lower.encoded[0] != upper.encoded[0] {
		return nil
	}
	if lower.op == cmpLT || lower.op == cmpLE {
		lower, upper = upper, lower
	}
	if (lower.op != cmpGT && lower.op != cmpGE) || (upper.op != cmpLT && upper.op != cmpLE) {
		return nil
	}
	lower.upperOp, lower.upper = upper.op, upper.encoded

	return queryPlan{lower}
}

func planFieldCall(field, q *gojq.Query, budget int) queryPlan {
	path, term := fieldPath(field), simpleTerm(q)
	if path == "" || term == nil || term.Type != gojq.TermTypeFunc {
		return nil
	}
	args := term.Func.Args
	switch term.Func.Name {
	case "endswith", "contains", "test":
		if match := stringMatcher(q, term.Func); match != nil {
			return queryPlan{{path: path, match: match}}
		}

		return nil
	}
	if len(args) != 1 {
		return nil
	}
	arg := args[0]
	switch term.Func.Name {
	case "IN":
		return planIn(path, arg, budget)
	case "startswith":
		scan, ok := planLiteral(path, cmpEq, arg)
		if ok && scan.encoded[0] == 0x04 {
			scan.prefix = true

			return queryPlan{scan}
		}
	}

	return nil
}

func stringMatcher(q *gojq.Query, fn *gojq.Func) func(context.Context, string) (bool, bool) {
	args := fn.Args
	if len(args) != 1 && (fn.Name != "test" || len(args) != 2) {
		return nil
	}
	for _, arg := range args {
		term := simpleTerm(arg)
		if term == nil || term.Type != gojq.TermTypeString || term.Str.Queries != nil {
			return nil
		}
	}

	literal := args[0].Term.Str.Str
	switch fn.Name {
	case "endswith":
		return func(_ context.Context, value string) (bool, bool) { return strings.HasSuffix(value, literal), true }
	case "contains":
		return func(_ context.Context, value string) (bool, bool) { return strings.Contains(value, literal), true }
	}

	// Use gojq for regex syntax and flags. Leave invalid patterns to the full query.
	code, err := gojq.Compile(q)
	if err != nil {
		return nil
	}
	if value, ok := code.Run("").Next(); !ok {
		return nil
	} else if _, ok := value.(bool); !ok {
		return nil
	}

	return func(ctx context.Context, value string) (bool, bool) {
		result, more := code.RunWithContext(ctx, value).Next()
		matched, valid := result.(bool)

		return matched, more && valid
	}
}

func planIn(path string, q *gojq.Query, budget int) queryPlan {
	if budget == 0 || !plainQuery(q) {
		return nil
	}
	if q.Op == gojq.OpComma {
		return planUnion(q, budget, func(q *gojq.Query, budget int) queryPlan {
			return planIn(path, q, budget)
		})
	}
	if scan, ok := planLiteral(path, cmpEq, q); ok {
		return queryPlan{scan}
	}

	return nil
}

func plainQuery(q *gojq.Query) bool {
	return q != nil && q.Meta == nil && len(q.Imports)+len(q.FuncDefs)+len(q.Patterns) == 0
}

func simpleTerm(q *gojq.Query) *gojq.Term {
	if !plainQuery(q) || q.Term == nil || len(q.Term.SuffixList) != 0 {
		return nil
	}

	return q.Term
}

func isField(term *gojq.Term) bool {
	return term != nil && term.Type == gojq.TermTypeIndex &&
		(term.Index.Name != "" || term.Index.Str != nil && term.Index.Str.Queries == nil)
}

func fieldPath(q *gojq.Query) string {
	term := simpleTerm(q)
	if !isField(term) {
		return ""
	}
	name := term.Index.Name
	if term.Index.Str != nil {
		name = term.Index.Str.Str
	}

	return "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(name)
}

func indexComparison(op gojq.Operator) (cmpOp, bool) {
	switch op {
	case gojq.OpEq:
		return cmpEq, true
	case gojq.OpLt:
		return cmpLT, true
	case gojq.OpLe:
		return cmpLE, true
	case gojq.OpGt:
		return cmpGT, true
	case gojq.OpGe:
		return cmpGE, true
	default:
		return 0, false
	}
}

func indexLiteral(term *gojq.Term) any {
	switch term.Type {
	case gojq.TermTypeNumber:
		return json.Number(term.Number)
	case gojq.TermTypeUnary:
		if inner := term.Unary.Term; term.Unary.Op == gojq.OpSub && inner.Type == gojq.TermTypeNumber && len(inner.SuffixList) == 0 {
			return json.Number("-" + inner.Number)
		}
	case gojq.TermTypeString:
		if term.Str.Queries == nil {
			return term.Str.Str
		}
	case gojq.TermTypeTrue:
		return true
	case gojq.TermTypeFalse:
		return false
	}

	// Null equality is not supported because missing fields have no index entry.
	return nil
}

func (s *store) scanEquality(database string, part indexScan, scan func(func(context.Context) (*queryDocument, error)) error) (used bool, err error) {
	err = s.withDocSnapshot(database, func(snapshot *pebble.Snapshot, key []byte) error {
		defs, err := s.indexes(database)
		if err != nil {
			return err
		}
		plan := queryPlan{part}
		names := planIndexNames(plan, defs)
		if names == nil {
			return nil
		}

		used = true
		return scanIndexDocs(snapshot, database, key, plan, names, scan)
	})

	return used, err
}

func (s *store) scanQuery(database string, plan queryPlan, scan func(func(context.Context) (*queryDocument, error)) error) error {
	return s.withDocSnapshot(database, func(snapshot *pebble.Snapshot, databaseKey []byte) error {
		if len(plan) != 0 {
			defs, err := s.indexes(database)
			if err != nil {
				return err
			}
			names := planIndexNames(plan, defs)
			if names == nil {
				return scanDocSnapshot(snapshot, database, databaseKey, scan)
			}
			return scanIndexDocs(snapshot, database, databaseKey, plan, names, scan)
		}

		return scanDocSnapshot(snapshot, database, databaseKey, scan)
	})
}

func planIndexNames(plan queryPlan, defs []indexDef) map[string]string {
	names := make(map[string]string, len(defs))
	for _, def := range defs {
		if names[def.path] == "" {
			names[def.path] = def.name
		}
	}
	for _, scan := range plan {
		if names[scan.path] == "" {
			return nil
		}
	}

	return names
}

func scanBounds(database, index string, scan indexScan) (pebble.IterOptions, []byte) {
	if scan.match != nil {
		prefix := indexTypePrefix(database, index, 0x04)

		return pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)}, prefix
	}
	if scan.prefix {
		// Drop the string terminator, but keep the type tag and escaped bytes.
		lower := indexValuePrefix(database, index, scan.encoded[:len(scan.encoded)-2])

		return pebble.IterOptions{LowerBound: lower, UpperBound: prefixEnd(lower)}, indexTypePrefix(database, index, 0x04)
	}
	bounds, prefix := indexBounds(database, index, scan.op, scan.encoded)
	if scan.upper != nil {
		upper, _ := indexBounds(database, index, scan.upperOp, scan.upper)
		bounds.UpperBound = upper.UpperBound
	}

	return bounds, prefix
}

func readIndexedDoc(snapshot *pebble.Snapshot, databaseKey, key []byte, id string) (doc *queryDocument, readErr error) {
	value, closer, err := snapshot.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, fmt.Errorf("%w: index references missing document", errCorruptData)
	}
	if err != nil {
		return nil, wrapStore("reading indexed document", err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			readErr = errors.Join(readErr, wrapStore("closing indexed document", err))
		}
	}()

	stored, err := openDoc(key, databaseKey, id, value)
	if err != nil {
		return nil, fmt.Errorf("reading indexed document: %w", err)
	}

	return &queryDocument{id: id, storedDoc: stored}, nil
}
