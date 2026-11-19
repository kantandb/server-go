package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"runtime/metrics"
	"time"

	"github.com/itchyny/gojq"
)

const (
	jqWorkerArg   = "--internal-jq-worker"
	jqMemoryLimit = 128 << 20
	jqMemoryExit  = 86
)

const (
	workerJSON byte = iota + 1
	workerStream
	workerReady
	workerRead
	workerDoc
	workerResult
	workerDone
	workerInvalid
	workerRuntime
	workerTimeout
	workerLimit
	workerCorrupt
)

var errQueryMemory = errors.New("jq query exceeded its memory limit")

// The worker owns no storage handles, keys, or inherited environment.
func init() {
	if len(os.Args) == 2 && os.Args[1] == jqWorkerArg {
		os.Exit(runWorker(os.Stdin, os.Stdout))
	}
}

type queryWorker struct {
	ctx    context.Context
	cmd    *exec.Cmd
	input  io.WriteCloser
	output io.ReadCloser
	log    workerLog
	waited bool
}

func startWorker(ctx context.Context, program []byte, stream bool) (*queryWorker, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding jq worker executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, jqWorkerArg)
	cmd.Env = []string{"GOTRACEBACK=none"}
	w := &queryWorker{ctx: ctx, cmd: cmd}
	cmd.Stderr = &w.log
	w.input, err = cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening jq worker input: %w", err)
	}
	w.output, err = cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("opening jq worker output: %w", err), w.input.Close())
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting jq worker: %w", err)
	}

	kind := workerJSON
	if stream {
		kind = workerStream
	}
	if err := w.send(kind, program); err != nil {
		defer w.stop()

		return nil, err
	}
	kind, payload, err := w.receive()
	if err != nil {
		defer w.stop()

		return nil, err
	}
	if kind != workerReady || len(payload) != 0 {
		defer w.stop()

		return nil, workerError(kind, payload)
	}

	return w, nil
}

func (w *queryWorker) scan(input *queryInput, emit func([]byte) error) error {
	for {
		kind, payload, err := w.receive()
		if err != nil {
			return err
		}
		switch kind {
		case workerRead:
			doc, err := input.readBody(w.ctx)
			if err != nil {
				return err
			}
			err = w.send(workerDoc, doc)
			clear(doc)
			if err != nil {
				return err
			}
		case workerResult:
			if err := emit(payload); err != nil {
				return err
			}
		case workerDone:
			return w.wait()
		default:
			return workerError(kind, payload)
		}
	}
}

func (w *queryWorker) receive() (byte, []byte, error) {
	kind, payload, err := readFrame(w.output, maxStoredDocBytes)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, nil, errors.Join(w.wait(), fmt.Errorf("reading jq worker frame: %w", err))
	}

	return kind, payload, err
}

func (w *queryWorker) send(kind byte, data []byte) error {
	if err := writeFrame(w.input, kind, data); err != nil {
		// A dead worker may close stdin before we read its exit status.
		_, _, receiveErr := w.receive()
		return errors.Join(err, receiveErr)
	}

	return nil
}

func (w *queryWorker) wait() error {
	w.waited = true
	err := w.cmd.Wait()
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == jqMemoryExit || bytes.Contains(w.log.data, []byte("out of memory"))) {
		return errQueryMemory
	}
	if err != nil {
		return fmt.Errorf("waiting for jq worker: %w", err)
	}

	return nil
}

func (w *queryWorker) stop() {
	if !w.waited {
		// Cancellation and exit can race. Wait still reaps the child.
		_ = w.cmd.Process.Kill()
		_ = w.wait()
	}
}

type workerLog struct{ data []byte }

func (l *workerLog) Write(p []byte) (int, error) {
	if left := 8192 - len(l.data); left > 0 {
		l.data = append(l.data, p[:min(len(p), left)]...)
	}

	return len(p), nil
}

func workerError(kind byte, detail []byte) error {
	var err error
	switch kind {
	case workerInvalid:
		err = errInvalidQuery
	case workerRuntime:
		err = errQueryRuntime
	case workerTimeout:
		return context.DeadlineExceeded
	case workerLimit:
		return errQueryLimit
	case workerCorrupt:
		err = errCorruptData
	default:
		return fmt.Errorf("unexpected jq worker frame %d", kind)
	}

	return fmt.Errorf("%w: %s", err, detail)
}

func runWorker(in io.Reader, out io.Writer) int {
	if err := boundWorker(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	debug.SetMemoryLimit(jqMemoryLimit * 3 / 4)
	go watchWorker()
	if err := evalWorker(in, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}

func watchWorker() {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	for range tick.C {
		checkWorkerMemory(samples)
	}
}

func checkWorkerMemory(samples []metrics.Sample) {
	metrics.Read(samples)
	if samples[0].Value.Uint64()-samples[1].Value.Uint64() > jqMemoryLimit {
		os.Exit(jqMemoryExit)
	}
}

func evalWorker(in io.Reader, out io.Writer) error {
	kind, program, err := readFrame(in, maxQueryBody)
	if err != nil {
		return err
	}
	if kind != workerJSON && kind != workerStream {
		return fmt.Errorf("unexpected jq request frame %d", kind)
	}
	input := &queryInput{read: func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := writeFrame(out, workerRead, nil); err != nil {
			return nil, err
		}
		kind, doc, err := readFrame(in, maxStoredDocBytes)
		if err == nil && kind != workerDoc {
			return nil, fmt.Errorf("unexpected jq input frame %d", kind)
		}

		return doc, err
	}}
	query, err := gojq.Parse(string(program))
	if err != nil {
		return writeWorkerError(out, workerInvalid, err)
	}
	code, err := gojq.Compile(query, gojq.WithInputIter(input))
	if err != nil {
		return writeWorkerError(out, workerInvalid, err)
	}
	if err := writeFrame(out, workerReady, nil); err != nil {
		return err
	}

	limit := maxStoredDocBytes
	if kind == workerJSON {
		limit = maxQueryBytes - 2
	}
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	for {
		done, err := evalQueryDoc(context.Background(), input, code, kind == workerStream, func(value []byte) error {
			checkWorkerMemory(samples)
			if len(value) > limit {
				return errQueryLimit
			}

			return writeFrame(out, workerResult, value)
		})
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return writeFrame(out, workerTimeout, nil)
		case errors.Is(err, errQueryLimit):
			return writeFrame(out, workerLimit, nil)
		case errors.Is(err, errCorruptData):
			return writeWorkerError(out, workerCorrupt, err)
		case errors.Is(err, errQueryRuntime):
			return writeWorkerError(out, workerRuntime, err)
		case err != nil:
			return err
		case done:
			return writeFrame(out, workerDone, nil)
		}
	}
}

func writeWorkerError(out io.Writer, kind byte, err error) error {
	detail := err.Error()
	return writeFrame(out, kind, []byte(detail[:min(len(detail), 4096)]))
}

func writeFrame(out io.Writer, kind byte, data []byte) error {
	header := [5]byte{kind}
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := out.Write(header[:]); err != nil {
		return fmt.Errorf("writing jq frame header: %w", err)
	}
	if len(data) > 0 {
		if _, err := out.Write(data); err != nil {
			return fmt.Errorf("writing jq frame body: %w", err)
		}
	}

	return nil
}

func readFrame(in io.Reader, limit uint32) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return 0, nil, fmt.Errorf("reading jq frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > limit {
		return 0, nil, fmt.Errorf("jq frame length %d exceeds limit %d", length, limit)
	}
	if length == 0 {
		return header[0], nil, nil
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(in, data); err != nil {
		return 0, nil, fmt.Errorf("reading jq frame body: %w", err)
	}

	return header[0], data, nil
}
