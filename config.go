package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	defaultMaxBodyBytes    int64 = 1 << 20
	defaultBulkMaxBytes    int64 = 64 << 20
	defaultBulkMaxDocs           = 1000
	defaultBulkBatchBytes        = 128 << 20
	defaultBulkTimeout           = 30 * time.Second
	defaultBulkConcurrency       = 1
)

type bulkConfig struct {
	maxBytes      int64
	maxDocuments  int
	maxBatchBytes int
	timeout       time.Duration
	concurrency   int
}

func defaultBulkConfig() bulkConfig {
	return bulkConfig{
		maxBytes:      defaultBulkMaxBytes,
		maxDocuments:  defaultBulkMaxDocs,
		maxBatchBytes: defaultBulkBatchBytes,
		timeout:       defaultBulkTimeout,
		concurrency:   defaultBulkConcurrency,
	}
}

type config struct {
	addr         string
	dataPath     string
	keyFile      string
	maxBodyBytes int64
	bulk         bulkConfig
}

func parseConfig(args []string) (config, error) {
	cfg := config{bulk: defaultBulkConfig()}
	flags := flag.NewFlagSet("kantan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.addr, "addr", ":8080", "HTTP listen address")
	flags.StringVar(&cfg.dataPath, "data", "data", "Pebble data directory")
	flags.StringVar(&cfg.keyFile, "key-file", "", "master-key file")
	flags.Int64Var(&cfg.maxBodyBytes, "max-body-bytes", defaultMaxBodyBytes, "maximum request body size")
	flags.Int64Var(&cfg.bulk.maxBytes, "bulk-max-bytes", defaultBulkMaxBytes, "maximum bulk import size")
	flags.IntVar(&cfg.bulk.maxDocuments, "bulk-max-documents", defaultBulkMaxDocs, "maximum documents per bulk import")
	flags.IntVar(&cfg.bulk.maxBatchBytes, "bulk-max-batch-bytes", defaultBulkBatchBytes, "maximum bulk Pebble batch size")
	flags.DurationVar(&cfg.bulk.timeout, "bulk-timeout", defaultBulkTimeout, "maximum bulk request duration")
	flags.IntVar(&cfg.bulk.concurrency, "bulk-concurrency", defaultBulkConcurrency, "maximum concurrent bulk requests")

	if err := flags.Parse(args); err != nil {
		return config{}, fmt.Errorf("parsing configuration: %w", err)
	}
	if cfg.addr == "" {
		return config{}, fmt.Errorf("validating configuration: address is empty")
	}
	if cfg.dataPath == "" {
		return config{}, fmt.Errorf("validating configuration: data path is empty")
	}
	if cfg.keyFile == "" {
		return config{}, fmt.Errorf("validating configuration: key file is empty")
	}
	if cfg.maxBodyBytes <= 0 || cfg.maxBodyBytes > maxStoredDocBytes {
		return config{}, fmt.Errorf("validating configuration: max body bytes must be between 1 and %d", maxStoredDocBytes)
	}
	if cfg.bulk.maxBytes <= 0 || cfg.bulk.maxDocuments <= 0 || cfg.bulk.maxBatchBytes <= 0 || cfg.bulk.timeout <= 0 || cfg.bulk.concurrency <= 0 {
		return config{}, fmt.Errorf("validating configuration: bulk limits must be positive")
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("validating configuration: unexpected arguments")
	}

	return cfg, nil
}

func loadMasterKey(path string) ([]byte, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, fmt.Errorf("decoding key file: invalid base64")
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("decoding key file: key must be %d bytes", keySize)
	}

	return key, nil
}
