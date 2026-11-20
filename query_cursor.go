package main

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

const maxQueryCursor = 16 << 10

func pageCipher(key []byte) (cipher.AEAD, error) {
	derived, err := deriveKey(key, []byte("kantan/query-cursor/v1"))
	if err != nil {
		return nil, fmt.Errorf("deriving query cursor key: %w", err)
	}
	defer clear(derived)

	return newGCM(derived)
}

func pageBinding(database string, program []byte, index string, defs []indexDef) ([]byte, error) {
	// Bind the token to the exact query, plan, and index definitions.
	parts := []string{"query-cursor/v1", database, string(program), index}
	for _, def := range defs {
		parts = append(parts, def.name, def.path)
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("encoding query cursor binding: %w", err)
	}
	hash := sha256.Sum256(encoded)

	return hash[:], nil
}

func encodeQueryCursor(box cipher.AEAD, binding, key []byte) (string, error) {
	nonce := make([]byte, box.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating query cursor nonce: %w", err)
	}
	sealed := box.Seal(nonce, nonce, key, binding)

	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decodeQueryCursor(box cipher.AEAD, binding []byte, token string) ([]byte, error) {
	if len(token) > maxQueryCursor {
		return nil, errQueryCursor
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) <= box.NonceSize()+box.Overhead() {
		return nil, errQueryCursor
	}
	key, err := box.Open(nil, sealed[:box.NonceSize()], sealed[box.NonceSize():], binding)
	if err != nil {
		return nil, errQueryCursor
	}

	return key, nil
}
