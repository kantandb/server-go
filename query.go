package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/theory/jsonpath"
	"github.com/theory/jsonpath/spec"
)

const (
	queryMethod     = "QUERY"
	maxQueryBody    = 64 << 10
	maxQueryPath    = 256
	maxPathDepth    = 16
	maxPathSegments = 32
	maxQueryNodes   = 1_000_000
	maxScanDocs     = 10_000
)

var (
	errInvalidPath = errors.New("invalid JSONPath")
	errQueryLimit  = errors.New("query exceeded the evaluation limit")
)

type pathQuery struct {
	index  string
	path   string
	value  any
	cursor string
	op     cmpOp
	limit  int
}

type pathPlan struct {
	path  *jsonpath.Path
	text  string
	index string
}

func decodePathQuery(body []byte) (pathQuery, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return pathQuery{}, errors.New("query must be an object")
	}

	query := pathQuery{op: cmpEq, limit: defaultListLimit}
	seen := make(map[string]bool, 6)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return pathQuery{}, err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return pathQuery{}, errors.New("invalid query field")
		}
		seen[name] = true

		switch name {
		case "index":
			err = decoder.Decode(&query.index)
		case "path":
			err = decoder.Decode(&query.path)
		case "op":
			var raw string
			if err = decoder.Decode(&raw); err == nil {
				var ok bool
				query.op, ok = parseCmpOp(raw)
				if !ok {
					err = errors.New("invalid operator")
				}
			}
		case "value":
			err = decoder.Decode(&query.value)
		case "limit":
			err = decoder.Decode(&query.limit)
		case "cursor":
			err = decoder.Decode(&query.cursor)
		default:
			return pathQuery{}, errors.New("unknown query field")
		}
		if err != nil {
			return pathQuery{}, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return pathQuery{}, err
	}
	if err := jsonEnd(decoder); err != nil {
		return pathQuery{}, err
	}
	filtered := seen["path"] || seen["index"]
	if filtered && (seen["path"] == seen["index"] || !seen["value"]) {
		return pathQuery{}, errors.New("exactly one of path or index, and value are required")
	}
	if !filtered && (seen["op"] || seen["value"]) {
		return pathQuery{}, errors.New("operator and value require a path or index")
	}
	if seen["path"] && query.path == "" || seen["index"] && validateName(query.index) != nil {
		return pathQuery{}, errors.New("invalid path or index")
	}
	if query.limit < 1 || query.limit > maxListLimit {
		return pathQuery{}, errors.New("invalid limit")
	}
	switch query.value.(type) {
	case nil, bool, json.Number, string:
	default:
		return pathQuery{}, errors.New("value must be scalar")
	}
	if query.op != cmpEq {
		switch query.value.(type) {
		case json.Number, string:
		default:
			return pathQuery{}, errors.New("operator requires a number or string")
		}
	}

	return query, nil
}

func jsonEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return err
	}

	return nil
}

// planQuery keeps named-index semantics separate from JSONPath evaluation.
func (s *store) planQuery(database string, query pathQuery) (pathPlan, error) {
	if query.index == "" && query.path != "" {
		return s.planPathQuery(database, query.path)
	}

	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	exists, err := s.hasDB(database)
	if err != nil {
		return pathPlan{}, fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return pathPlan{}, errDBNotFound
	}
	if query.index == "" {
		return pathPlan{}, nil
	}
	defs, err := s.indexes(database)
	if err != nil {
		return pathPlan{}, err
	}
	if _, ok := findIndex(defs, query.index); !ok {
		return pathPlan{}, errIndexNotFound
	}

	return pathPlan{index: query.index}, nil
}

func (s *store) planPathQuery(database, raw string) (pathPlan, error) {
	if err := validatePathText(raw); err != nil {
		return pathPlan{}, err
	}
	path, err := jsonpath.Parse(raw)
	if err != nil {
		return pathPlan{}, fmt.Errorf("%w: %v", errInvalidPath, err)
	}
	if len(path.Query().Segments()) > maxPathSegments || !safePath(path.String()) {
		return pathPlan{}, errInvalidPath
	}
	plan := pathPlan{path: path, text: path.String()}

	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	exists, err := s.hasDB(database)
	if err != nil {
		return pathPlan{}, fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return pathPlan{}, errDBNotFound
	}
	pointer, ok := jsonPathPointer(path)
	if !ok {
		return plan, nil
	}

	defs, err := s.indexes(database)
	if err != nil {
		return pathPlan{}, err
	}
	for _, def := range defs {
		if def.path == pointer {
			plan.index = def.name

			break
		}
	}

	return plan, nil
}

func validatePathText(path string) error {
	if len(path) == 0 || len(path) > maxQueryPath || !utf8.ValidString(path) {
		return errInvalidPath
	}

	depth, maxDepth := 0, 0
	var quote byte
	escaped := false
	for i := range len(path) {
		char := path[i]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '[', '(':
			depth++
			maxDepth = max(maxDepth, depth)
		case ']', ')':
			depth--
		}
	}
	if maxDepth > maxPathDepth {
		return errInvalidPath
	}

	return nil
}

func safePath(path string) bool {
	var quote byte
	var delimiters []byte
	escaped := false
	descendants := 0
	for i := range len(path) {
		char := path[i]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '[', '(':
			delimiters = append(delimiters, char)
		case ']', ')':
			delimiters = delimiters[:len(delimiters)-1]
		case ',':
			if delimiters[len(delimiters)-1] == '[' {
				return false
			}
		case '.':
			if i > 0 && path[i-1] == '.' {
				descendants++
			}
		}
	}

	return descendants <= 1
}

func jsonPathPointer(path *jsonpath.Path) (string, bool) {
	var pointer strings.Builder
	for _, segment := range path.Query().Segments() {
		selectors := segment.Selectors()
		if segment.IsDescendant() || len(selectors) != 1 {
			return "", false
		}

		switch selector := selectors[0].(type) {
		case spec.Name:
			name := string(selector)
			if index, err := strconv.Atoi(name); err == nil && index >= 0 && strconv.Itoa(index) == name {
				return "", false
			}
			name = strings.ReplaceAll(name, "~", "~0")
			name = strings.ReplaceAll(name, "/", "~1")
			pointer.WriteByte('/')
			pointer.WriteString(name)
		case spec.Index:
			return "", false
		default:
			return "", false
		}
	}

	result := pointer.String()

	return result, result != ""
}

func pathMatches(ctx context.Context, path *jsonpath.Path, root any, op cmpOp, expected []byte, maxNodes int) (bool, error) {
	if err := checkQueryNodes(ctx, root, maxNodes); err != nil {
		return false, err
	}

	values := path.Select(root)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, value := range values {
		actual, err := encodeIndexValue(value)
		if err != nil || actual[0] != expected[0] {
			continue
		}

		order := bytes.Compare(actual, expected)
		if op == cmpEq && order == 0 || op == cmpLT && order < 0 || op == cmpLE && order <= 0 || op == cmpGT && order > 0 || op == cmpGE && order >= 0 {
			return true, nil
		}
	}

	return false, nil
}

func checkQueryNodes(ctx context.Context, root any, limit int) error {
	stack := []any{root}
	for work := 0; len(stack) > 0; work++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if work >= limit {
			return errQueryLimit
		}

		last := len(stack) - 1
		value := stack[last]
		stack = stack[:last]
		switch value := value.(type) {
		case []any:
			stack = append(stack, value...)
		case map[string]any:
			for _, child := range value {
				stack = append(stack, child)
			}
		}
	}

	return nil
}
