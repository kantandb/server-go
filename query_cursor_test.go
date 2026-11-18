package main

import (
	"crypto/cipher"
	"testing"
)

const testCursorID = "01950000-0000-7000-8000-000000000001"

func valueForCursor(t *testing.T, value any) []byte {
	t.Helper()

	encoded, err := encodeIndexValue(value)
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}

	return encoded
}

func testQueryCipher(t *testing.T) cipher.AEAD {
	t.Helper()

	box, err := newQueryCipher(make([]byte, cursorKeySize))
	if err != nil {
		t.Fatalf("newQueryCipher() error = %v", err)
	}

	return box
}
