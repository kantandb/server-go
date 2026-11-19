package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type api struct {
	store        *store
	maxBodyBytes int64
	bulk         bulkConfig
	bulkSlots    chan struct{}
	querySlots   chan struct{}
	log          *slog.Logger
	stopping     atomic.Bool
}

type dbRequest struct {
	Name    string          `json:"name"`
	Indices *[]indexRequest `json:"indices,omitempty"`
}

type indexRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (r dbRequest) indexDefs() []indexDef {
	if r.Indices == nil {
		return nil
	}

	defs := make([]indexDef, len(*r.Indices))
	for i, index := range *r.Indices {
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
	return newAPIWithBulk(store, maxBodyBytes, defaultBulkConfig(), log)
}

func newAPIWithBulk(store *store, maxBodyBytes int64, bulk bulkConfig, log *slog.Logger) *api {
	return &api{
		store:        store,
		maxBodyBytes: maxBodyBytes,
		bulk:         bulk,
		bulkSlots:    make(chan struct{}, bulk.concurrency),
		querySlots:   make(chan struct{}, queryConcurrency),
		log:          log,
	}
}

func (a *api) takeBulkSlot() bool {
	select {
	case a.bulkSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *api) freeBulkSlot() {
	<-a.bulkSlots
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
	route(mux, http.MethodGet, "/db/{database}", a.getDB)
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
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body must contain a database name and optional indices")

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

func (a *api) getDB(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", queryMediaType)

	database := r.PathValue("database")
	if err := validateName(database); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "URI query parameters are not supported")

		return
	}

	info, err := a.store.databaseInfo(r.Context(), database)
	if errors.Is(err, errDBNotFound) {
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")

		return
	}
	if err != nil {
		a.fail(w, r, "get database", err)

		return
	}

	writeJSON(w, http.StatusOK, info)
}

func (a *api) queryOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", queryMediaType)
	w.Header().Set("Allow", "GET, POST, QUERY, DELETE, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) queryDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Query", queryMediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept")

	database := r.PathValue("database")
	if err := validateName(database); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid")

		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "URI query parameters are not supported")

		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if charset := params["charset"]; err != nil || mediaType != queryMediaType || charset != "" && !strings.EqualFold(charset, "utf-8") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be text/plain with UTF-8 encoding")

		return
	}

	select {
	case a.querySlots <- struct{}{}:
		defer func() { <-a.querySlots }()
	default:
		writeError(w, http.StatusTooManyRequests, "query_busy", "Too many queries")

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
	responseType := queryResponseType(r.Header.Values("Accept"))
	if responseType == "" {
		writeError(w, http.StatusNotAcceptable, "not_acceptable", "Accept must allow application/json or application/x-ndjson")

		return
	}

	if responseType == queryNDJSON {
		a.streamQuery(w, r, database, body)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	result, err := a.store.queryArray(ctx, database, body)
	if err != nil {
		a.failQuery(w, r, err)

		return
	}
	writeData(w, http.StatusOK, queryJSON+"; charset=utf-8", result)
}

func (a *api) failQuery(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errInvalidQuery):
		writeError(w, http.StatusBadRequest, "invalid_query", "Query is invalid")
	case errors.Is(err, errQueryRuntime):
		writeError(w, http.StatusBadRequest, "query_runtime", "Query execution failed")
	case errors.Is(err, errDBNotFound):
		writeError(w, http.StatusNotFound, "database_not_found", "Database does not exist")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "query_timeout", "Query exceeded the execution limit")
	case errors.Is(err, errQueryLimit):
		writeError(w, http.StatusServiceUnavailable, "query_limit", "Query exceeded the evaluation limit")
	case errors.Is(err, context.Canceled):
		return
	default:
		a.fail(w, r, "query documents", err)
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

func (a *api) importBulk(w http.ResponseWriter, r *http.Request) {
	ctx, done, ok := a.startBulk(w, r)
	if !ok {
		return
	}
	defer done()

	documents, err := readBulkCtx(ctx, r.Body, a.maxBodyBytes, a.bulk.maxBytes, a.bulk.maxDocuments)
	if err != nil {
		a.failBulk(w, r, "read bulk import", err)

		return
	}

	if _, err := a.store.importDocs(ctx, r.PathValue("database"), documents, a.bulk.maxBatchBytes); err != nil {
		a.failBulk(w, r, "import documents", err)

		return
	}

	writeBulkSuccess(w)
}

func (a *api) startBulk(w http.ResponseWriter, r *http.Request) (context.Context, func(), bool) {
	if err := validateName(r.PathValue("database")); err != nil {
		writeBulkError(w, http.StatusBadRequest, "invalid_name", "Database name is invalid", 0)

		return nil, nil, false
	}
	if r.URL.RawQuery != "" {
		writeBulkError(w, http.StatusBadRequest, "invalid_query", "URI query parameters are not supported", 0)

		return nil, nil, false
	}
	if !a.takeBulkSlot() {
		a.log.Warn("bulk request rejected", "method", r.Method, "path", r.URL.Path)
		writeBulkError(w, http.StatusTooManyRequests, "bulk_limit", "Too many bulk requests", 0)

		return nil, nil, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.bulk.timeout)
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)

	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		cancel()
		a.freeBulkSlot()
		a.failBulk(w, r, "set network deadline", err)

		return nil, nil, false
	}

	done := func() {
		if ctx.Err() == nil {
			_ = controller.SetReadDeadline(time.Time{})
		}
		cancel()
		a.freeBulkSlot()
	}

	return ctx, done, true
}

func (a *api) failBulk(w http.ResponseWriter, r *http.Request, operation string, err error) {
	a.log.Error("request failed", "operation", operation, "method", r.Method, "path", r.URL.Path, "error", err)

	line := 0
	if lineErr, ok := errors.AsType[*bulkLineError](err); ok {
		line = lineErr.line
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeBulkError(w, http.StatusServiceUnavailable, "bulk_timeout", "Bulk request exceeded the execution limit", line)
	case errors.Is(err, errBulkLimit), errors.Is(err, errBodyTooLarge):
		writeBulkError(w, http.StatusRequestEntityTooLarge, "content_too_large", "Bulk import exceeds a size or count limit", line)
	case errors.Is(err, errBulkEmpty):
		writeBulkError(w, http.StatusBadRequest, "invalid_document", "Import must contain a document", line)
	case errors.Is(err, errInvalidDoc):
		writeBulkError(w, http.StatusBadRequest, "invalid_document", "Document must be a JSON object", line)
	case errors.Is(err, errInvalidIndexValue):
		writeBulkError(w, http.StatusBadRequest, "invalid_document", "An indexed value exceeds the size limit", line)
	case errors.Is(err, errBulkRead):
		writeBulkError(w, http.StatusBadRequest, "invalid_document", "Could not read import", line)
	case errors.Is(err, errDBNotFound):
		writeBulkError(w, http.StatusNotFound, "database_not_found", "Database does not exist", line)
	case errors.Is(err, errCorruptData):
		writeBulkError(w, http.StatusInternalServerError, "corrupt_data", "Stored data is corrupt", line)
	case isStoreUnavailable(err):
		writeBulkError(w, http.StatusServiceUnavailable, "service_unavailable", "Service is unavailable", line)
	default:
		writeBulkError(w, http.StatusInternalServerError, "internal_error", "Internal server error", line)
	}
}

func (a *api) createDoc(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if isNDJSON(contentType) {
		a.importBulk(w, r)

		return
	}
	if !isJSON(contentType) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json or application/x-ndjson")

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
		Indices json.RawMessage `json:"indices"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return dbRequest{}, err
	}
	if bytes.Equal(bytes.TrimSpace(fields.Indices), []byte("null")) {
		return dbRequest{}, errors.New("indices must be an array")
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
