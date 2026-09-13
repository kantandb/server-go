package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

const (
	bulkMediaType  = "application/x-ndjson"
	bulkBufferSize = 32 << 10
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

		document, validateErr := validateDoc(line, maxLine)
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

type bulkStream struct {
	writer   http.ResponseWriter
	buffered *bufio.Writer
	pending  int
	started  bool
}

func newBulkStream(writer http.ResponseWriter) *bulkStream {
	return &bulkStream{writer: writer, buffered: bufio.NewWriterSize(writer, bulkBufferSize)}
}

func (s *bulkStream) write(document []byte) error {
	if !s.started {
		s.writer.WriteHeader(http.StatusOK)
		s.started = true
	}

	if _, err := s.buffered.Write(document); err != nil {
		return err
	}
	if err := s.buffered.WriteByte('\n'); err != nil {
		return err
	}

	s.pending += len(document) + 1
	if s.pending < bulkBufferSize {
		return nil
	}

	return s.flush()
}

func (s *bulkStream) flush() error {
	if err := s.buffered.Flush(); err != nil {
		return err
	}
	s.pending = 0

	return http.NewResponseController(s.writer).Flush()
}

func writeBulkSuccess(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, bulkResponse{Success: true})
}

func writeBulkError(w http.ResponseWriter, status int, code, message string, line int) {
	writeJSON(w, status, bulkResponse{Error: bulkErrorBody{Code: code, Line: line, Message: message}})
}
