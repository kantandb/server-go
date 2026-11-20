package main

import (
	"errors"
	"net/http"
	"time"
)

const (
	queryStatus     = "Query-Status"
	queryBufferSize = 32 << 10
)

func (a *api) streamQuery(w http.ResponseWriter, r *http.Request, database string, program []byte, page *queryPage) {
	controller := http.NewResponseController(w)
	started, complete := false, false
	start := func() {
		w.Header().Set("Content-Type", queryNDJSON)
		w.Header().Add("Trailer", queryStatus)
		if page != nil {
			w.Header().Add("Trailer", "Cursor")
		}
		w.WriteHeader(http.StatusOK)
		started = true
	}
	defer func() {
		if started {
			status := "error"
			if complete {
				status = "ok"
			}
			w.Header().Set(queryStatus, status)
		}
	}()

	write := func(chunk []byte) error {
		// Bound stalled writes without limiting the duration of a full scan.
		deadlineErr := controller.SetWriteDeadline(time.Now().Add(queryTimeout))
		if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
			return deadlineErr
		}
		if !started {
			start()
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		if deadlineErr == nil {
			return controller.SetWriteDeadline(time.Time{})
		}

		return nil
	}

	var pending []byte
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		chunk := pending
		pending = pending[:0]

		return write(chunk)
	}
	emit := func(value []byte) error {
		// Send the first line immediately. Keep later batches bounded.
		if !started || len(value) >= queryBufferSize {
			if err := flush(); err != nil {
				return err
			}

			return write(append(value, '\n'))
		}
		if len(pending)+len(value)+1 > queryBufferSize {
			if err := flush(); err != nil {
				return err
			}
		}
		if pending == nil {
			pending = make([]byte, 0, queryBufferSize)
		}
		pending = append(pending, value...)
		pending = append(pending, '\n')

		return nil
	}

	err := a.store.runQueryPage(r.Context(), database, program, true, page, emit)
	// Keep values emitted before a query error.
	if flushErr := flush(); flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		if !started {
			a.failQuery(w, r, err)
		} else {
			a.log.Warn("query stream failed", "path", r.URL.Path, "error", err)
		}

		return
	}
	if !started {
		start()
	}
	if page != nil && page.next != "" {
		w.Header().Set("Cursor", page.next)
	}
	complete = true
}
