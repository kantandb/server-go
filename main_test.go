package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestRunStopsWithContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := config{
		addr:         "127.0.0.1:0",
		dataPath:     t.TempDir(),
		keyFile:      writeTestKey(t),
		maxBodyBytes: defaultMaxBodyBytes,
		bulk:         defaultBulkConfig(),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := run(ctx, cfg, log); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}
