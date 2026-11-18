package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

var errInvalidQueryCursor = errors.New("invalid query cursor")

func newQueryCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cursor cipher: %w", err)
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating cursor GCM: %w", err)
	}

	return box, nil
}

func validIndexValue(value []byte) bool {
	if len(value) == 0 || len(value) > maxEncodedIndexValue {
		return false
	}

	switch value[0] {
	case 0x00, 0x01, 0x02:
		return len(value) == 1
	case 0x03:
		return validSortableNumber(value[1:])
	case 0x04:
		return validSortableString(value[1:])
	default:
		return false
	}
}

func validSortableNumber(value []byte) bool {
	if len(value) == 1 {
		return value[0] == 0x01
	}
	if len(value) < 11 || value[0] != 0x00 && value[0] != 0x02 {
		return false
	}

	negative := value[0] == 0x00
	orderedExponent := binary.BigEndian.Uint64(value[1:9])
	if negative {
		orderedExponent = ^orderedExponent
	}
	exponent := int64(orderedExponent ^ (uint64(1) << 63))
	if exponent < -maxIndexValue || exponent > maxIndexValue || len(value)-10 > maxIndexValue {
		return false
	}

	terminator, zero := byte(0x00), byte(0x01)
	if negative {
		terminator, zero = 0xff, 0xfe
	}
	if value[len(value)-1] != terminator || value[9] == zero || value[len(value)-2] == zero {
		return false
	}
	for _, digit := range value[9 : len(value)-1] {
		if !negative && (digit < 0x01 || digit > 0x0a) || negative && (digit < 0xf5 || digit > 0xfe) {
			return false
		}
	}

	return true
}

func validSortableString(value []byte) bool {
	decoded := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != 0x00 {
			decoded = append(decoded, value[i])
			continue
		}
		if i+1 >= len(value) {
			return false
		}
		switch value[i+1] {
		case 0x00:
			return i+2 == len(value) && len(decoded)+1 <= maxIndexValue && utf8.Valid(decoded)
		case 0xff:
			decoded = append(decoded, 0x00)
			i++
		default:
			return false
		}
	}

	return false
}
