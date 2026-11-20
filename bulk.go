package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"
)

const (
	bulkMediaType = "application/x-ndjson"
)

var (
	errBulkEmpty = errors.New("bulk import is empty")
	errBulkLimit = errors.New("bulk import limit exceeded")
	errBulkRead  = errors.New("could not read bulk import")
)

type bulkLineError struct {
	line int
	err  error
}

func (e *bulkLineError) Error() string {
	return fmt.Sprintf("line %d: %v", e.line, e.err)
}

func (e *bulkLineError) Unwrap() error {
	return e.err
}

type bulkResponse struct {
	Success bool          `json:"success"`
	Error   bulkErrorBody `json:"error"`
}

type bulkErrorBody struct {
	Code    string `json:"code,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message,omitempty"`
}

func isNDJSON(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)

	return err == nil && mediaType == bulkMediaType
}

func readBulk(reader io.Reader, maxLine, maxBytes int64, maxDocs int) ([][]byte, error) {
	return readBulkCtx(context.Background(), reader, maxLine, maxBytes, maxDocs)
}

func readBulkCtx(ctx context.Context, reader io.Reader, maxLine, maxBytes int64, maxDocs int) ([][]byte, error) {
	buffered := bufio.NewReader(contextReader{ctx: ctx, reader: reader})
	var documents [][]byte
	var line []byte
	var total int64
	var decoder objectDecoder
	lineNumber := 1

	for {
		fragment, err := buffered.ReadSlice('\n')
		total += int64(len(fragment))
		if total > maxBytes {
			return nil, errBulkLimit
		}

		line = append(line, fragment...)
		if int64(lineLen(line, err == nil)) > maxLine {
			return nil, &bulkLineError{line: lineNumber, err: errBulkLimit}
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				return nil, context.DeadlineExceeded
			}

			return nil, fmt.Errorf("%w: %w", errBulkRead, err)
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}

		line = trimLineEnd(line, err == nil)
		if len(line) == 0 {
			return nil, &bulkLineError{line: lineNumber, err: errInvalidDoc}
		}
		if len(documents) >= maxDocs {
			return nil, &bulkLineError{line: lineNumber, err: errBulkLimit}
		}

		document, validateErr := decoder.validate(line, maxLine)
		if validateErr != nil {
			return nil, &bulkLineError{line: lineNumber, err: validateErr}
		}
		document, validateErr = unwrapImport(document, &decoder, maxLine)
		if validateErr != nil {
			return nil, &bulkLineError{line: lineNumber, err: validateErr}
		}
		documents = append(documents, document)
		line = nil
		lineNumber++

		if errors.Is(err, io.EOF) {
			break
		}
	}

	if len(documents) == 0 {
		return nil, errBulkEmpty
	}

	return documents, nil
}

// Only the exact export shape is unwrapped; other objects remain documents.
func unwrapImport(body []byte, decoder *objectDecoder, maxBytes int64) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decoding import document: %w", err)
	}
	var id, rev string
	if len(fields) != 3 || fields["doc"] == nil ||
		len(fields["id"]) == 0 || fields["id"][0] != '"' ||
		len(fields["rev"]) == 0 || fields["rev"][0] != '"' ||
		json.Unmarshal(fields["id"], &id) != nil || json.Unmarshal(fields["rev"], &rev) != nil {
		return body, nil
	}

	return decoder.validate(fields["doc"], maxBytes)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	return r.reader.Read(buffer)
}

func lineLen(line []byte, complete bool) int {
	length := len(line)
	if complete && length > 0 && line[length-1] == '\n' {
		length--
		if length > 0 && line[length-1] == '\r' {
			length--
		}
	}

	return length
}

func trimLineEnd(line []byte, complete bool) []byte {
	if !complete {
		return line
	}

	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})

	return line
}

func writeBulkSuccess(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, bulkResponse{Success: true})
}

func writeBulkError(w http.ResponseWriter, status int, code, message string, line int) {
	writeJSON(w, status, bulkResponse{Error: bulkErrorBody{Code: code, Line: line, Message: message}})
}
