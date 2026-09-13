package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

func TestParseConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    config
		wantErr bool
	}{
		{
			name: "defaults",
			args: []string{"-key-file", "key"},
			want: config{addr: ":8080", dataPath: "data", keyFile: "key", maxBodyBytes: defaultMaxBodyBytes, bulk: defaultBulkConfig()},
		},
		{
			name: "custom",
			args: []string{
				"-addr", "127.0.0.1:9000", "-data", "/tmp/kantan", "-key-file", "/tmp/key", "-max-body-bytes", "2048",
				"-bulk-max-bytes", "4096", "-bulk-max-documents", "12", "-bulk-max-batch-bytes", "8192",
				"-bulk-timeout", "2m", "-bulk-concurrency", "3",
			},
			want: config{
				addr: "127.0.0.1:9000", dataPath: "/tmp/kantan", keyFile: "/tmp/key", maxBodyBytes: 2048,
				bulk: bulkConfig{maxBytes: 4096, maxDocuments: 12, maxBatchBytes: 8192, timeout: 2 * time.Minute, concurrency: 3},
			},
		},
		{name: "empty address", args: []string{"-addr", "", "-key-file", "key"}, wantErr: true},
		{name: "empty data path", args: []string{"-data", "", "-key-file", "key"}, wantErr: true},
		{name: "missing key file", wantErr: true},
		{name: "zero body limit", args: []string{"-max-body-bytes", "0", "-key-file", "key"}, wantErr: true},
		{name: "excessive body limit", args: []string{"-max-body-bytes", "67108865", "-key-file", "key"}, wantErr: true},
		{name: "zero bulk bytes", args: []string{"-bulk-max-bytes", "0", "-key-file", "key"}, wantErr: true},
		{name: "zero bulk documents", args: []string{"-bulk-max-documents", "0", "-key-file", "key"}, wantErr: true},
		{name: "zero bulk batch", args: []string{"-bulk-max-batch-bytes", "0", "-key-file", "key"}, wantErr: true},
		{name: "zero bulk timeout", args: []string{"-bulk-timeout", "0s", "-key-file", "key"}, wantErr: true},
		{name: "zero bulk concurrency", args: []string{"-bulk-concurrency", "0", "-key-file", "key"}, wantErr: true},
		{name: "unexpected argument", args: []string{"-key-file", "key", "extra"}, wantErr: true},
		{name: "unknown flag", args: []string{"-unknown"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseConfig(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseConfig() error = nil, want error")
				}

				return
			}
			if err != nil {
				t.Fatalf("parseConfig() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("parseConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadMasterKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: base64.StdEncoding.EncodeToString(testMasterKey) + "\n"},
		{name: "malformed", value: "not-base64", wantErr: true},
		{name: "short", value: base64.StdEncoding.EncodeToString(testMasterKey[:31]), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, []byte(tt.value), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			got, err := loadMasterKey(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadMasterKey() error = nil, want error")
				}

				return
			}
			if err != nil {
				t.Fatalf("loadMasterKey() error = %v", err)
			}
			if string(got) != string(testMasterKey) {
				t.Error("loadMasterKey() returned wrong key")
			}
		})
	}
}

func writeTestKey(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "key")
	encoded := base64.StdEncoding.EncodeToString(testMasterKey)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	return path
}
