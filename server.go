package main

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type api struct {
	store        *store
	maxBodyBytes int64
	log          *slog.Logger
	stopping     atomic.Bool
}

type dbRequest struct {
	Name    string          `json:"name"`
	Indexes *[]indexRequest `json:"indexes,omitempty"`
}

type indexRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (r dbRequest) indexDefs() []indexDef {
	if r.Indexes == nil {
		return nil
	}

	defs := make([]indexDef, len(*r.Indexes))
	for i, index := range *r.Indexes {
		defs[i] = indexDef{name: index.Name, path: index.Path}
	}

	return defs
}

type serviceInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type dbList struct {
	Databases []string `json:"databases"`
}

type docList struct {
	Documents []string `json:"documents"`
	Cursor    string   `json:"cursor"`
}

type cmpOp byte

const (
	cmpEq cmpOp = iota
	cmpLT
	cmpLE
	cmpGT
	cmpGE
)

func (op cmpOp) valid() bool {
	return op <= cmpGE
}

type docQuery struct {
	value   any
	cursor  string
	index   string
	op      cmpOp
	limit   int
	indexed bool
}

type docResponse struct {
	ID string `json:"id"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("encoding response: %w", err))
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		panic(fmt.Errorf("writing response: %w", err))
	}
}

func writeData(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		panic(fmt.Errorf("writing response: %w", err))
	}
}

func newHandler(store *store, maxBodyBytes int64) http.Handler {
	return newAPI(store, maxBodyBytes, slog.New(slog.DiscardHandler)).handler()
}

func newAPI(store *store, maxBodyBytes int64, log *slog.Logger) *api {
	return &api{store: store, maxBodyBytes: maxBodyBytes, log: log}
}

const (
	defaultListLimit = 100
	maxListLimit     = 1000
	queryTimeout     = 5 * time.Second
)

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	route(mux, http.MethodGet, "/{$}", a.welcome)
	route(mux, http.MethodGet, "/healthz", a.health)
	route(mux, http.MethodPost, "/db", a.createDB)
	route(mux, http.MethodGet, "/db", a.listDBs)
	route(mux, http.MethodGet, "/db/{database}", a.listDocs)
	route(mux, queryMethod, "/db/{database}", a.queryDocs)
	route(mux, http.MethodOptions, "/db/{database}", a.queryOptions)
	route(mux, http.MethodDelete, "/db/{database}", a.deleteDB)
	route(mux, http.MethodPost, "/db/{database}", a.createDoc)
	route(mux, http.MethodGet, "/db/{database}/{id}", a.getDoc)
	route(mux, http.MethodPut, "/db/{database}/{id}", a.replaceDoc)
	route(mux, http.MethodPatch, "/db/{database}/{id}", a.patchDoc)
	route(mux, http.MethodDelete, "/db/{database}/{id}", a.deleteDoc)

	for _, pattern := range []string{"/{$}", "/healthz", "/db", "/db/{database}", "/db/{database}/{id}"} {
		mux.HandleFunc(pattern, methodNotAllowed)
	}
	mux.HandleFunc("/", notFound)

	return a.recover(a.rejectStopping(rejectUncleanPath(mux)))
}

func route(mux *http.ServeMux, method, pattern string, handler http.HandlerFunc) {
	mux.HandleFunc(method+" "+pattern, handler)
	if method == http.MethodGet {
		mux.HandleFunc(http.MethodHead+" "+pattern, methodNotAllowed)
	}
	if pattern != "/{$}" {
		mux.HandleFunc(method+" "+pattern+"/{$}", redirectNoSlash)
	}
}

func redirectNoSlash(w http.ResponseWriter, r *http.Request) {
	status := http.StatusTemporaryRedirect
	if r.Method == http.MethodGet {
		status = http.StatusMovedPermanently
	}

	location := strings.TrimSuffix(r.URL.Path, "/")
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, location, status)
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not allowed")
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "route_not_found", "Route does not exist")
}

func rejectUncleanPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "//") {
			notFound(w, r)

			return
		}
		for part := range strings.SplitSeq(r.URL.Path, "/") {
			if part == "." || part == ".." {
				notFound(w, r)

				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (a *api) stop() {
	a.stopping.Store(true)
}

type responseState struct {
	http.ResponseWriter
	written bool
}

func (w *responseState) WriteHeader(status int) {
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseState) Write(body []byte) (int, error) {
	w.written = true

	return w.ResponseWriter.Write(body)
}

func (w *responseState) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (a *api) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &responseState{ResponseWriter: w}
		defer func() {
			value := recover()
			if value == nil {
				return
			}

			err, ok := value.(error)
			if !ok {
				err = fmt.Errorf("panic: %v", value)
			}
			a.log.Error("request panic", "method", r.Method, "path", r.URL.Path, "error", err, "stack", string(debug.Stack()))
			if !state.written {
				writeFailure(state, err)
			}
		}()

		next.ServeHTTP(state, r)
	})
}

func (a *api) rejectStopping(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.stopping.Load() {
			writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Service is unavailable")

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (a *api) welcome(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, serviceInfo{Name: "KantanDB", Version: buildVersion})
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	_ = a.store.db.Metrics()

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) createDB(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}

	body, err := readBody(r.Body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Request body exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not read request body")

		return
	}

	request, err := decodeDBRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body must contain a database name and optional indexes")

		return
	}
	if err := validateName(request.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}

	err = a.store.createDB(request.Name, request.indexDefs()...)
	if errors.Is(err, errDBExists) {
		writeError(w, http.StatusConflict, "database_exists", "Database already exists")

		return
	}
	if errors.Is(err, errInvalidIndex) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Index definitions are invalid")

		return
	}
	if err != nil {
		a.fail(w, r, "create database", err)

		return
	}

	w.Header().Set("Location", "/db/"+request.Name)
	writeJSON(w, http.StatusCreated, request)
}

func (a *api) listDBs(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := parseListQuery(w, r, validateName)
	if !ok {
		return
	}

	names, err := a.store.listDBs(limit, cursor)
	if err != nil {
		a.fail(w, r, "list databases", err)

		return
	}
	if names == nil {
		names = []string{}
	}

	writeJSON(w, http.StatusOK, dbList{Databases: names})
}

func (a *api) listDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", "application/json")

	database := r.PathValue("database")
	if err := validateName(database); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}

	query, ok := parseDocQuery(w, r)
	if !ok {
		return
	}

	var ids []string
	var encoded []byte
	var lastValue []byte
	var box cipher.AEAD
	var more bool
	var err error
	if query.indexed {
		var key []byte
		key, err = a.store.cursorKey(database)
		if err == nil {
			box, err = newQueryCipher(key)
		}

		var cursor string
		if err == nil {
			encoded, lastValue, cursor, err = queryStart(box, database, query)
		}
		if errors.Is(err, errInvalidQueryCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Cursor is invalid")

			return
		}
		if err == nil {
			if query.op == cmpEq {
				ids, more, err = a.store.queryDocsCtx(r.Context(), database, query.index, encoded, query.limit, cursor)
			} else {
				page, queryErr := a.store.queryRangePage(r.Context(), database, query.index, query.op, encoded, query.limit, lastValue, cursor)
				ids, lastValue, more, err = page.ids, page.lastValue, page.more, queryErr
			}
		}
	} else {
		ids, more, err = a.store.listDocs(database, query.limit, query.cursor)
	}
	if errors.Is(err, errDBNotFound) {
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")

		return
	}
	if errors.Is(err, errIndexNotFound) {
		writeError(w, http.StatusNotFound, "index_not_found", "Index does not exist")

		return
	}
	if errors.Is(err, errInvalidIndexValue) {
		writeError(w, http.StatusBadRequest, "invalid_query", "Query is invalid")

		return
	}
	if err != nil {
		a.fail(w, r, "list documents", err)

		return
	}

	if ids == nil {
		ids = []string{}
	}

	cursor := ""
	if more && query.indexed {
		cursor, err = encodeQueryCursor(box, queryCursor{database: database, index: query.index, op: query.op, value: encoded, lastValue: lastValue, id: ids[len(ids)-1]})
		if err != nil {
			a.fail(w, r, "encode query cursor", err)

			return
		}
	} else if more {
		cursor = ids[len(ids)-1]
	}

	writeJSON(w, http.StatusOK, docList{Documents: ids, Cursor: cursor})
}

func (a *api) queryOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", "application/json")
	w.Header().Set("Allow", "GET, POST, QUERY, DELETE, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) queryDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	database := r.PathValue("database")
	if err := validateName(database); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "URI query parameters are not supported")

		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}

	body, err := readBody(r.Body, min(a.maxBodyBytes, int64(maxQueryBody)))
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Request body exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not read request body")

		return
	}
	query, err := decodePathQuery(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "Query body is invalid")

		return
	}
	plan, err := a.store.planPathQuery(database, query.path)
	if errors.Is(err, errInvalidPath) {
		writeError(w, http.StatusBadRequest, "invalid_query", "Path is invalid")

		return
	}

	var ids []string
	var cursor string
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()

		ids, cursor, err = a.store.executePathQuery(ctx, database, plan, query)
	}
	if errors.Is(err, errInvalidQueryCursor) {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "Cursor is invalid")

		return
	}
	if errors.Is(err, errDBNotFound) {
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")

		return
	}
	if errors.Is(err, errInvalidIndexValue) {
		writeError(w, http.StatusBadRequest, "invalid_query", "Query is invalid")

		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "query_timeout", "Query exceeded the execution limit")

		return
	}
	if errors.Is(err, errQueryLimit) {
		writeError(w, http.StatusServiceUnavailable, "query_limit", "Query exceeded the evaluation limit")

		return
	}
	if err != nil {
		a.fail(w, r, "query documents", err)

		return
	}
	if ids == nil {
		ids = []string{}
	}

	writeJSON(w, http.StatusOK, docList{Documents: ids, Cursor: cursor})
}

func queryStart(box cipher.AEAD, database string, query docQuery) ([]byte, []byte, string, error) {
	encoded, err := encodeIndexValue(query.value)
	if err != nil {
		return nil, nil, "", err
	}
	if query.cursor == "" {
		return encoded, nil, "", nil
	}

	cursor, err := decodeQueryCursor(box, query.cursor)
	if err != nil || cursor.database != database || cursor.index != query.index || cursor.op != query.op || !bytes.Equal(cursor.value, encoded) {
		return nil, nil, "", errInvalidQueryCursor
	}

	return encoded, cursor.lastValue, cursor.id, nil
}

func parseDocQuery(w http.ResponseWriter, r *http.Request) (docQuery, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "Query is invalid")

		return docQuery{}, false
	}
	for name, entries := range values {
		if name != "limit" && name != "cursor" && name != "index" && name != "op" && name != "value" || len(entries) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "Query is invalid")

			return docQuery{}, false
		}
	}

	query := docQuery{op: cmpEq, limit: defaultListLimit}
	if entries, ok := values["limit"]; ok {
		limit, err := strconv.Atoi(entries[0])
		if err != nil || limit < 1 || limit > maxListLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be between 1 and 1000")

			return docQuery{}, false
		}
		query.limit = limit
	}
	if entries, ok := values["cursor"]; ok {
		query.cursor = entries[0]
	}

	indexes, hasIndex := values["index"]
	rawValues, hasValue := values["value"]
	ops, hasOp := values["op"]
	if hasIndex != hasValue {
		writeError(w, http.StatusBadRequest, "invalid_query", "Index and value must appear together")

		return docQuery{}, false
	}
	if hasOp && !hasIndex {
		writeError(w, http.StatusBadRequest, "invalid_query", "Operator requires index and value")

		return docQuery{}, false
	}
	if !hasIndex {
		if query.cursor != "" {
			if err := validateID(query.cursor); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Cursor is invalid")

				return docQuery{}, false
			}
		}

		return query, true
	}
	if err := validateName(indexes[0]); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "Index is invalid")

		return docQuery{}, false
	}

	value, err := decodeQueryValue(rawValues[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "Value must be one JSON scalar")

		return docQuery{}, false
	}
	if hasOp {
		var ok bool
		query.op, ok = parseCmpOp(ops[0])
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_query", "Operator is invalid")

			return docQuery{}, false
		}
	}
	if query.op != cmpEq {
		switch value.(type) {
		case nil, bool:
			writeError(w, http.StatusBadRequest, "invalid_query", "Operator does not support this value")

			return docQuery{}, false
		}
	}

	query.index = indexes[0]
	query.value = value
	query.indexed = true

	return query, true
}

func parseCmpOp(raw string) (cmpOp, bool) {
	switch raw {
	case "eq":
		return cmpEq, true
	case "lt":
		return cmpLT, true
	case "le":
		return cmpLE, true
	case "gt":
		return cmpGT, true
	case "ge":
		return cmpGE, true
	default:
		return cmpEq, false
	}
}

func decodeQueryValue(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("multiple JSON values")
	}

	switch value.(type) {
	case nil, bool, json.Number, string:
		return value, nil
	default:
		return nil, errors.New("value must be scalar")
	}
}

func parseListQuery(w http.ResponseWriter, r *http.Request, validateCursor func(string) error) (int, string, bool) {
	limit := defaultListLimit
	if values, ok := r.URL.Query()["limit"]; ok {
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > maxListLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be between 1 and 1000")

			return 0, "", false
		}
		limit = parsed
	}

	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		if err := validateCursor(cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Cursor is invalid")

			return 0, "", false
		}
	}

	return limit, cursor, true
}

func (a *api) deleteDB(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("database")
	if err := validateName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}

	if err := a.store.deleteDB(name); errors.Is(err, errDBNotFound) {
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")

		return
	} else if err != nil {
		a.fail(w, r, "delete database", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *api) createDoc(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}

	database := r.PathValue("database")
	if err := validateName(database); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}

	body, err := readBody(r.Body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Request body exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_document", "Could not read document")

		return
	}

	document, err := validateDoc(body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Document exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_document", "Document must be a JSON object")

		return
	}

	id, rev, err := a.store.createDoc(database, document)
	if errors.Is(err, errDBNotFound) {
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")

		return
	}
	if errors.Is(err, errInvalidIndexValue) {
		writeError(w, http.StatusBadRequest, "invalid_document", "An indexed value exceeds the size limit")

		return
	}
	if err != nil {
		a.fail(w, r, "create document", err)

		return
	}

	w.Header().Set("Location", "/db/"+database+"/"+id)
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusCreated, docResponse{ID: id})
}

func (a *api) getDoc(w http.ResponseWriter, r *http.Request) {
	if !validDocPath(w, r) {
		return
	}

	doc, err := a.store.getDoc(r.PathValue("database"), r.PathValue("id"))
	if errors.Is(err, errDocNotFound) {
		writeError(w, http.StatusNotFound, "document_not_found", "Document does not exist")

		return
	}
	if err != nil {
		a.fail(w, r, "read document", err)

		return
	}

	w.Header().Set("ETag", formatETag(doc.revision))
	writeData(w, http.StatusOK, "application/json", doc.json)
}

func (a *api) replaceDoc(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}
	if !validDocPath(w, r) {
		return
	}

	match, err := parseIfMatch(r.Header.Values("If-Match"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is invalid")

		return
	}

	body, err := readBody(r.Body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Request body exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_document", "Could not read document")

		return
	}

	document, err := validateDoc(body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Document exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_document", "Document must be a JSON object")

		return
	}

	rev, err := a.store.replaceDoc(r.PathValue("database"), r.PathValue("id"), document, match)
	if errors.Is(err, errDocNotFound) {
		writeError(w, http.StatusNotFound, "document_not_found", "Document does not exist")

		return
	}
	if errors.Is(err, errPreconditionFailed) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match precondition failed")

		return
	}
	if errors.Is(err, errInvalidIndexValue) {
		writeError(w, http.StatusBadRequest, "invalid_document", "An indexed value exceeds the size limit")

		return
	}
	if err != nil {
		a.fail(w, r, "replace document", err)

		return
	}

	w.Header().Set("ETag", formatETag(rev))
	writeData(w, http.StatusOK, "application/json", document)
}

func (a *api) patchDoc(w http.ResponseWriter, r *http.Request) {
	mediaType, err := parsePatchType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must select a supported patch format")

		return
	}
	if !validDocPath(w, r) {
		return
	}

	match, err := parseIfMatch(r.Header.Values("If-Match"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is invalid")

		return
	}

	body, err := readBody(r.Body, a.maxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Request body exceeds the size limit")

		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_patch", "Could not read patch")

		return
	}

	doc, err := a.store.patchDoc(r.PathValue("database"), r.PathValue("id"), match, func(document []byte) ([]byte, error) {
		return applyPatch(document, body, mediaType, a.maxBodyBytes)
	})
	if errors.Is(err, errDocNotFound) {
		writeError(w, http.StatusNotFound, "document_not_found", "Document does not exist")

		return
	}
	if errors.Is(err, errPreconditionFailed) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match precondition failed")

		return
	}
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Document exceeds the size limit")

		return
	}
	if errors.Is(err, errInvalidPatch) {
		writeError(w, http.StatusBadRequest, "invalid_patch", "Patch is invalid or produces a non-object document")

		return
	}
	if errors.Is(err, errInvalidIndexValue) {
		writeError(w, http.StatusBadRequest, "invalid_patch", "An indexed value exceeds the size limit")

		return
	}
	if err != nil {
		a.fail(w, r, "patch document", err)

		return
	}

	w.Header().Set("ETag", formatETag(doc.revision))
	writeData(w, http.StatusOK, "application/json", doc.json)
}

func (a *api) deleteDoc(w http.ResponseWriter, r *http.Request) {
	if !validDocPath(w, r) {
		return
	}

	match, err := parseIfMatch(r.Header.Values("If-Match"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is invalid")

		return
	}

	err = a.store.deleteDoc(r.PathValue("database"), r.PathValue("id"), match)
	if errors.Is(err, errDocNotFound) {
		writeError(w, http.StatusNotFound, "document_not_found", "Document does not exist")

		return
	}
	if errors.Is(err, errPreconditionFailed) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match precondition failed")

		return
	}
	if err != nil {
		a.fail(w, r, "delete document", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func validDocPath(w http.ResponseWriter, r *http.Request) bool {
	if err := validateName(r.PathValue("database")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return false
	}
	if err := validateID(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Document ID is invalid")

		return false
	}

	return true
}

func decodeDBRequest(body []byte) (dbRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var request dbRequest
	if err := decoder.Decode(&request); err != nil {
		return dbRequest{}, err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return dbRequest{}, errors.New("multiple JSON values")
		}

		return dbRequest{}, err
	}

	var fields struct {
		Indexes json.RawMessage `json:"indexes"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return dbRequest{}, err
	}
	if bytes.Equal(bytes.TrimSpace(fields.Indexes), []byte("null")) {
		return dbRequest{}, errors.New("indexes must be an array")
	}

	return request, nil
}

func isJSON(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)

	return err == nil && mediaType == "application/json"
}

func (a *api) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	a.log.Error("request failed", "operation", operation, "method", r.Method, "path", r.URL.Path, "error", err)
	writeFailure(w, err)
}

func writeFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCorruptData):
		writeError(w, http.StatusInternalServerError, "corrupt_data", "Stored data is corrupt")
	case isStoreUnavailable(err):
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Service is unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}
