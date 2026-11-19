package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

var (
	errInvalidName    = errors.New("invalid database name")
	errInvalidID      = errors.New("invalid document ID")
	errInvalidDoc     = errors.New("invalid document")
	errBodyTooLarge   = errors.New("body too large")
	errInvalidETag    = errors.New("invalid ETag")
	errInvalidIfMatch = errors.New("invalid If-Match")
)

type matchCond struct {
	set      bool
	wildcard bool
	revision revision
}

func validateName(name string) error {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return errInvalidName
	}

	for i := 1; i < len(name); i++ {
		if validNameChar(name[i]) {
			continue
		}

		return errInvalidName
	}

	return nil
}

func validateID(id string) error {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return errInvalidID
	}

	for i := range len(id) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !lowerHex(id[i]) {
			return errInvalidID
		}
	}
	if id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
		return errInvalidID
	}

	return nil
}

func readBody(reader io.Reader, maxBytes int64) ([]byte, error) {
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}

	body, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}

	return body, nil
}

func validateDoc(body []byte, maxBytes int64) ([]byte, error) {
	var decoder objectDecoder

	return decoder.validate(body, maxBytes)
}

func (d *objectDecoder) validate(body []byte, maxBytes int64) ([]byte, error) {
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("%w: invalid UTF-8", errInvalidDoc)
	}

	value, err := d.decode(body, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidDoc, err)
	}

	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding document: %w", err)
	}
	if int64(len(canonical)) > maxBytes {
		return nil, errBodyTooLarge
	}

	return canonical, nil
}

func formatETag(rev revision) string {
	return `"` + hex.EncodeToString(rev[:]) + `"`
}

func parseETag(value string) (revision, error) {
	if len(value) != 34 || value[0] != '"' || value[len(value)-1] != '"' {
		return revision{}, errInvalidETag
	}

	encoded := value[1 : len(value)-1]
	for i := range len(encoded) {
		if !lowerHex(encoded[i]) {
			return revision{}, errInvalidETag
		}
	}

	var rev revision
	if _, err := hex.Decode(rev[:], []byte(encoded)); err != nil {
		return revision{}, fmt.Errorf("%w: %v", errInvalidETag, err)
	}

	return rev, nil
}

func parseIfMatch(values []string) (matchCond, error) {
	if len(values) == 0 {
		return matchCond{}, nil
	}
	if len(values) != 1 {
		return matchCond{}, errInvalidIfMatch
	}

	value := strings.TrimSpace(values[0])
	if value == "*" {
		return matchCond{set: true, wildcard: true}, nil
	}

	rev, err := parseETag(value)
	if err != nil {
		return matchCond{}, fmt.Errorf("%w: %v", errInvalidIfMatch, err)
	}

	return matchCond{set: true, revision: rev}, nil
}

func validNameChar(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-'
}

func lowerHex(char byte) bool {
	return char >= '0' && char <= '9' || char >= 'a' && char <= 'f'
}
