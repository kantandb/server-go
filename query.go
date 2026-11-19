package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/itchyny/gojq"
)

const (
	queryMethod    = "QUERY"
	queryMediaType = "text/plain"
	queryJSON      = "application/json"
	queryNDJSON    = bulkMediaType
	maxQueryBody   = 64 << 10
	maxScanDocs    = 10_000
	maxScanBytes   = 64 << 20
	maxQueryBytes  = 8 << 20
)

var (
	errInvalidQuery = errors.New("invalid jq program")
	errQueryRuntime = errors.New("jq execution failed")
	errQueryLimit   = errors.New("query exceeded the evaluation limit")
)

type queryInput struct {
	ctx     context.Context
	read    func(context.Context) ([]byte, error)
	docs    int
	bytes   int
	limit   bool
	done    bool
	err     error
	decoder objectDecoder
}

// Next shares the scan between the main loop and jq's input and inputs.
func (in *queryInput) Next() (any, bool) {
	if in.done {
		return nil, false
	}

	value, err := in.next()
	in.err, in.done = err, value == nil
	if err != nil {
		return err, true
	}

	return value, !in.done
}

func (in *queryInput) next() (any, error) {
	body, err := in.readBody(in.ctx)
	if err != nil || body == nil {
		return nil, err
	}
	defer clear(body)

	value, err := in.decoder.decode(body, true)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid document JSON", errCorruptData)
	}

	return value, nil
}

func (in *queryInput) readBody(ctx context.Context) ([]byte, error) {
	body, err := in.read(ctx)
	if err != nil || body == nil {
		return nil, err
	}
	if in.limit {
		if in.docs == maxScanDocs || len(body) > maxScanBytes-in.bytes {
			clear(body)

			return nil, errQueryLimit
		}
		in.docs++
		in.bytes += len(body)
	}

	return body, nil
}

func (s *store) runQuery(ctx context.Context, database string, program []byte, stream bool, emit func([]byte) error) error {
	if !utf8.Valid(program) || len(bytes.TrimSpace(program)) == 0 {
		return errInvalidQuery
	}
	query, err := gojq.Parse(string(program))
	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidQuery, err)
	}

	if direct := planDirect(query); direct != nil {
		return s.runDirect(ctx, direct, database, stream, emit)
	}

	plan := planQuery(query)
	input := &queryInput{limit: !stream}
	// No module or environment loader. Only the document scan is exposed.
	code, err := gojq.Compile(query, gojq.WithInputIter(input))
	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidQuery, err)
	}

	return s.scanQuery(ctx, database, plan, func(read func(context.Context) ([]byte, error)) error {
		input.read = read
		for {
			done, err := evalQueryDoc(ctx, input, code, stream, emit)
			if done || err != nil {
				return err
			}
		}
	})
}

func evalQueryDoc(ctx context.Context, input *queryInput, code *gojq.Code, stream bool, emit func([]byte) error) (bool, error) {
	input.ctx = ctx
	if stream {
		evalCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		input.ctx = evalCtx
	}

	return runQueryDoc(input, code, emit)
}

func runQueryDoc(input *queryInput, code *gojq.Code, emit func([]byte) error) (bool, error) {
	doc, ok := input.Next()
	if !ok || input.err != nil {
		return true, input.err
	}

	results := code.RunWithContext(input.ctx, doc)
	for {
		value, ok := results.Next()
		// jq can catch errors. A failed storage read must still fail the request.
		if input.err != nil {
			return false, input.err
		}
		if err := input.ctx.Err(); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if err, failed := value.(error); failed {
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.ExitCode() == 0 {
				return true, nil
			}

			return false, fmt.Errorf("%w: %w", errQueryRuntime, err)
		}

		encoded, err := gojq.Marshal(value)
		if err != nil {
			return false, fmt.Errorf("encoding jq output: %w", err)
		}
		if err := emit(encoded); err != nil {
			return false, err
		}
	}
}

func (s *store) queryArray(ctx context.Context, database string, program []byte) ([]byte, error) {
	body := []byte{'['}
	err := s.runQuery(ctx, database, program, false, func(value []byte) error {
		if len(body) > 1 {
			body = append(body, ',')
		}
		if len(value) > maxQueryBytes-len(body)-1 {
			return errQueryLimit
		}
		body = append(body, value...)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return append(body, ']'), nil
}
