package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/itchyny/gojq"
)

type directPlan struct {
	field   string
	project bool
}

func planDirect(q *gojq.Query) *directPlan {
	term := simpleTerm(q)
	if term == nil {
		return nil
	}
	if term.Type == gojq.TermTypeIdentity {
		return &directPlan{}
	}
	if !isField(term) {
		return nil
	}
	if term.Index.Str != nil {
		return &directPlan{field: term.Index.Str.Str, project: true}
	}

	return &directPlan{field: term.Index.Name, project: true}
}

func (s *store) runDirect(ctx context.Context, plan *directPlan, database string, stream bool, emit func([]byte) error) error {
	return s.scanDocs(database, func(read func(context.Context) ([]byte, error)) error {
		input := queryInput{read: read, limit: !stream}
		for {
			body, err := input.readBody(ctx)
			if err != nil || body == nil {
				return err
			}
			err = plan.emit(body, emit)
			clear(body)
			if err != nil {
				return err
			}
		}
	})
}

func (plan *directPlan) emit(body []byte, emit func([]byte) error) error {
	if !plan.project {
		return emit(body)
	}

	value, err := storedField(body, plan.field)
	if err != nil {
		return err
	}

	return emit(value)
}

// Stored objects are compact and have unique keys. Skip unselected values without decoding them.
func storedField(body []byte, field string) ([]byte, error) {
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, fmt.Errorf("%w: invalid document JSON", errCorruptData)
	}
	for i := 1; i < len(body)-1; {
		if body[i] != '"' {
			return nil, fmt.Errorf("%w: invalid document key", errCorruptData)
		}
		end := stringEnd(body, i+1)
		if end == len(body) || body[end] != ':' {
			return nil, fmt.Errorf("%w: invalid document key", errCorruptData)
		}
		key := body[i+1 : end-1]
		match := string(key) == field
		if bytes.IndexByte(key, '\\') >= 0 {
			var decoded string
			if err := json.Unmarshal(body[i:end], &decoded); err != nil {
				return nil, fmt.Errorf("%w: invalid document key: %w", errCorruptData, err)
			}
			match = decoded == field
		}

		start := end + 1
		end = fieldEnd(body, start)
		if end == start || end == len(body) {
			return nil, fmt.Errorf("%w: invalid document value", errCorruptData)
		}
		if match {
			return body[start:end:end], nil
		}
		i = end + 1
	}

	return []byte("null"), nil
}

func stringEnd(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case '"':
			return i + 1
		case '\\':
			i++
		}
		i++
	}

	return len(body)
}

func fieldEnd(body []byte, i int) int {
	depth := 0
	for i < len(body) {
		switch body[i] {
		case '"':
			i = stringEnd(body, i+1)

			continue
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i
			}
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
		i++
	}

	return len(body)
}
