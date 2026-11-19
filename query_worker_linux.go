package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func boundWorker() error {
	// Allow for Go's reserved address space, but bound new mappings.
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return fmt.Errorf("reading jq worker address space: %w", err)
	}
	size, _, _ := strings.Cut(string(data), " ")
	pages, err := strconv.ParseUint(size, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing jq worker address space: %w", err)
	}
	limit := pages*uint64(os.Getpagesize()) + 2*jqMemoryLimit
	var current unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &current); err != nil {
		return fmt.Errorf("reading jq worker address-space limit: %w", err)
	}
	current.Cur = min(current.Cur, limit)
	current.Max = min(current.Max, limit)
	if err := unix.Setrlimit(unix.RLIMIT_AS, &current); err != nil {
		return fmt.Errorf("setting jq worker address-space limit: %w", err)
	}

	return nil
}
