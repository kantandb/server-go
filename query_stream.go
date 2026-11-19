package main

import (
	"errors"
	"net/http"
	"time"
)

const queryStatus = "Query-Status"

func (a *api) streamQuery(w http.ResponseWriter, r *http.Request, database string, program []byte) {
	controller := http.NewResponseController(w)
	started, complete := false, false
	start := func() {
		w.Header().Set("Content-Type", queryNDJSON)
		w.Header().Add("Trailer", queryStatus)
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

	emit := func(value []byte) error {
		// Bound stalled writes without limiting the duration of a full scan.
		deadlineErr := controller.SetWriteDeadline(time.Now().Add(queryTimeout))
		if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
			return deadlineErr
		}
		if !started {
			start()
		}
		if _, err := w.Write(append(value, '\n')); err != nil {
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

	if err := a.store.runQuery(r.Context(), database, program, true, emit); err != nil {
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
	complete = true
}
