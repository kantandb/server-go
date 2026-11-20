package main

import "github.com/itchyny/gojq"

// Accept only syntax with at most one output and no cross-document state.
func pageQuery(q *gojq.Query, depth int) bool {
	if depth == 0 || !plainQuery(q) {
		return false
	}
	if q.Term == nil {
		switch q.Op {
		case gojq.OpPipe, gojq.OpAnd, gojq.OpOr,
			gojq.OpEq, gojq.OpNe, gojq.OpLt, gojq.OpLe, gojq.OpGt, gojq.OpGe:
			return pageQuery(q.Left, depth-1) && pageQuery(q.Right, depth-1)
		default:
			return false
		}
	}

	t := q.Term
	for _, suffix := range t.SuffixList {
		if suffix.Iter || suffix.Optional || !pageIndex(suffix.Index) {
			return false
		}
	}
	switch t.Type {
	case gojq.TermTypeIdentity, gojq.TermTypeNull, gojq.TermTypeTrue, gojq.TermTypeFalse, gojq.TermTypeNumber:
		return true
	case gojq.TermTypeIndex:
		return pageIndex(t.Index)
	case gojq.TermTypeString:
		return t.Str.Queries == nil
	case gojq.TermTypeQuery:
		return pageQuery(t.Query, depth-1)
	case gojq.TermTypeUnary:
		return len(t.Unary.Term.SuffixList) == 0 && t.Unary.Term.Type == gojq.TermTypeNumber
	case gojq.TermTypeArray:
		return t.Array.Query == nil || pageArray(t.Array.Query, depth-1)
	case gojq.TermTypeObject:
		for _, pair := range t.Object.KeyVals {
			if pair.KeyQuery != nil || pair.KeyString != nil && pair.KeyString.Queries != nil ||
				pair.Val != nil && !pageQuery(pair.Val, depth-1) {
				return false
			}
		}

		return true
	case gojq.TermTypeFunc:
		return pageFunc(t.Func, depth-1)
	default:
		return false
	}
}

func pageIndex(index *gojq.Index) bool {
	return index != nil && (index.Name != "" || index.Str != nil && index.Str.Queries == nil) &&
		index.Start == nil && index.End == nil && !index.IsSlice
}

// Commas collect values inside an array without producing multiple results.
func pageArray(q *gojq.Query, depth int) bool {
	if depth == 0 || !plainQuery(q) {
		return false
	}
	if q.Op == gojq.OpComma {
		return pageArray(q.Left, depth-1) && pageArray(q.Right, depth-1)
	}

	return pageQuery(q, depth-1)
}

func pageFunc(fn *gojq.Func, depth int) bool {
	switch fn.Name {
	case "select", "startswith", "endswith", "contains":
		return len(fn.Args) == 1 && pageQuery(fn.Args[0], depth)
	case "test":
		return (len(fn.Args) == 1 || len(fn.Args) == 2) && pageArgs(fn.Args, depth)
	case "IN":
		return len(fn.Args) == 1 && pageArray(fn.Args[0], depth)
	case "length", "type", "not", "ascii_downcase", "ascii_upcase":
		return len(fn.Args) == 0
	default:
		return false
	}
}

func pageArgs(args []*gojq.Query, depth int) bool {
	for _, arg := range args {
		if !pageQuery(arg, depth) {
			return false
		}
	}

	return true
}
