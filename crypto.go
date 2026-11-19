package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble"
	"golang.org/x/crypto/hkdf"
)

var (
	storeMetaKey       = []byte{0x00}
	errInvalidStoreKey = errors.New("missing or incorrect storage key")
	storeVerifier      = sha256.Sum256([]byte("kantandb/store/verifier/v1"))
)

const (
	wrappingInfo = "kantandb/wrapping/v1"
	documentInfo = "kantandb/document/v1"
	storeADInfo  = "kantandb/store-metadata/v1"
	databaseAD   = "kantandb/database-record/v1"
	documentAD   = "kantandb/document-record/v1"
)

func initStoreCrypto(db *pebble.DB, masterKey []byte) ([]byte, error) {
	if len(masterKey) != keySize {
		return nil, errInvalidStoreKey
	}

	value, closer, err := db.Get(storeMetaKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return initStoreMeta(db, masterKey)
	}
	if err != nil {
		return nil, wrapStore("reading store metadata", err)
	}
	key, openErr := openStoreMeta(value, masterKey)
	if err := closer.Close(); err != nil {
		clear(key)

		return nil, wrapStore("closing store metadata", err)
	}
	if openErr != nil {
		return nil, errInvalidStoreKey
	}

	return key, nil
}

func initStoreMeta(db *pebble.DB, masterKey []byte) ([]byte, error) {
	empty, err := storeEmpty(db)
	if err != nil {
		return nil, err
	}
	if !empty {
		return nil, errInvalidStoreKey
	}

	salt := make([]byte, storeSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating store salt: %w", err)
	}
	key, err := deriveWrapKey(masterKey, salt)
	if err != nil {
		return nil, err
	}
	box, err := newGCM(key)
	if err != nil {
		clear(key)

		return nil, err
	}

	header := make([]byte, storeMetaHeaderLen)
	header[0] = storeMetaVersion
	header[1] = wrappingKey1
	copy(header[2:], salt)
	nonce := header[2+storeSaltLen:]
	if _, err := rand.Read(nonce); err != nil {
		clear(key)

		return nil, fmt.Errorf("generating store nonce: %w", err)
	}
	value := box.Seal(header, nonce, storeVerifier[:], recordAD(storeADInfo, storeMetaKey, header))
	if err := db.Set(storeMetaKey, value, pebble.Sync); err != nil {
		clear(key)

		return nil, wrapStore("writing store metadata", err)
	}

	return key, nil
}

func openStoreMeta(value, masterKey []byte) ([]byte, error) {
	if len(value) != storeMetaHeaderLen+len(storeVerifier)+gcmTagLen || value[0] != storeMetaVersion || value[1] != wrappingKey1 {
		return nil, errInvalidStoreKey
	}

	header := value[:storeMetaHeaderLen]
	key, err := deriveWrapKey(masterKey, header[2:2+storeSaltLen])
	if err != nil {
		return nil, err
	}
	box, err := newGCM(key)
	if err != nil {
		clear(key)

		return nil, err
	}

	nonce := header[2+storeSaltLen:]
	plain, err := box.Open(nil, nonce, value[storeMetaHeaderLen:], recordAD(storeADInfo, storeMetaKey, header))
	if err != nil || subtle.ConstantTimeCompare(plain, storeVerifier[:]) != 1 {
		clear(key)
		clear(plain)

		return nil, errInvalidStoreKey
	}
	clear(plain)

	return key, nil
}

func storeEmpty(db *pebble.DB) (empty bool, resultErr error) {
	iter, err := db.NewIter(nil)
	if err != nil {
		return false, wrapStore("checking empty store", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			resultErr = errors.Join(resultErr, wrapStore("closing store iterator", err))
		}
	}()

	empty = !iter.First()
	if err := iter.Error(); err != nil {
		return false, wrapStore("iterating store", err)
	}

	return empty, nil
}

func deriveWrapKey(masterKey, salt []byte) ([]byte, error) {
	reader := hkdf.New(sha256.New, masterKey, salt, []byte(wrappingInfo))
	key := make([]byte, keySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("deriving wrapping key: %w", err)
	}

	return key, nil
}

func deriveDocumentKey(databaseKey []byte, id string) ([]byte, error) {
	info := make([]byte, len(documentInfo)+8+len(id))
	copy(info, documentInfo)
	binary.BigEndian.PutUint64(info[len(documentInfo):], uint64(len(id)))
	copy(info[len(documentInfo)+8:], id)

	return deriveKey(databaseKey, info)
}

func deriveKey(input, info []byte) ([]byte, error) {
	if len(input) != keySize {
		return nil, errors.New("invalid key length")
	}

	reader := hkdf.New(sha256.New, input, nil, info)
	key := make([]byte, keySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("deriving purpose key: %w", err)
	}

	return key, nil
}

func wrapDBKey(wrappingKey, pebbleKey, databaseKey []byte) ([]byte, error) {
	if len(databaseKey) != keySize {
		return nil, errors.New("invalid database key length")
	}
	box, err := newGCM(wrappingKey)
	if err != nil {
		return nil, err
	}

	header := make([]byte, dbRecordHeaderLen)
	header[0] = dbRecordVersion
	header[1] = wrappingKey1
	nonce := header[2:]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating database-key nonce: %w", err)
	}

	return box.Seal(header, nonce, databaseKey, recordAD(databaseAD, pebbleKey, header)), nil
}

func unwrapDBKey(wrappingKey, pebbleKey, value []byte) ([]byte, error) {
	if len(value) != dbRecordHeaderLen+keySize+gcmTagLen || value[0] != dbRecordVersion || value[1] != wrappingKey1 {
		return nil, errCorruptData
	}
	box, err := newGCM(wrappingKey)
	if err != nil {
		return nil, err
	}

	header := value[:dbRecordHeaderLen]
	key, err := box.Open(nil, header[2:], value[dbRecordHeaderLen:], recordAD(databaseAD, pebbleKey, header))
	if err != nil || len(key) != keySize {
		clear(key)

		return nil, errCorruptData
	}

	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating AES-GCM: %w", err)
	}

	return box, nil
}

func recordAD(domain string, key, header []byte) []byte {
	ad := make([]byte, len(domain)+8+len(key)+len(header))
	copy(ad, domain)
	binary.BigEndian.PutUint64(ad[len(domain):], uint64(len(key)))
	copy(ad[len(domain)+8:], key)
	copy(ad[len(domain)+8+len(key):], header)

	return ad
}
