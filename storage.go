package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

var (
	errDBExists           = errors.New("database exists")
	errDBNotFound         = errors.New("database not found")
	errDocExists          = errors.New("document exists")
	errDocNotFound        = errors.New("document not found")
	errPreconditionFailed = errors.New("precondition failed")
	errCorruptData        = errors.New("corrupt stored data")
	errStoreUnavailable   = errors.New("storage unavailable")
)

var (
	dbPrefix      = []byte{0x01}
	docPrefix     = []byte{0x02}
	idxDefPrefix  = []byte{0x03}
	idxDataPrefix = []byte{0x04}
)

const cursorKeySize = keySize

const (
	dbStripeCount  = 64
	docStripeCount = 256
	fnvOffset      = 14695981039346656037
	fnvPrime       = 1099511628211
)

type revision [16]byte

type storedDoc struct {
	json     []byte
	revision revision
}

type store struct {
	db          *pebble.DB
	wrappingKey []byte

	// Database locks precede document locks so deletion excludes active writes.
	dbStripes  [dbStripeCount]sync.RWMutex
	docStripes [docStripeCount]sync.Mutex
}

func storeOptions() *pebble.Options {
	return &pebble.Options{
		Levels: []pebble.LevelOptions{{
			FilterPolicy: bloom.FilterPolicy(10),
		}},
	}
}

func openStore(path string, masterKey []byte) (*store, error) {
	db, err := pebble.Open(path, storeOptions())
	if err != nil {
		return nil, fmt.Errorf("opening Pebble: %w", err)
	}

	wrappingKey, err := initStoreCrypto(db, masterKey)
	if err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing Pebble: %w", closeErr))
		}

		return nil, err
	}

	return &store{db: db, wrappingKey: wrappingKey}, nil
}

func (s *store) close() error {
	clear(s.wrappingKey)

	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing Pebble: %w", err)
	}

	return nil
}

func (s *store) createDB(name string, defs ...indexDef) (createErr error) {
	if err := validateIndexes(defs); err != nil {
		return err
	}

	dbMu := s.dbLock(name)
	dbMu.Lock()
	defer dbMu.Unlock()

	exists, err := s.hasDB(name)
	if err != nil {
		return fmt.Errorf("checking database: %w", err)
	}
	if exists {
		return errDBExists
	}

	record, err := s.makeDBRecord(dbKey(name))
	if err != nil {
		return err
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			createErr = errors.Join(createErr, wrapStore("closing database batch", err))
		}
	}()

	if err := batch.Set(dbKey(name), record, nil); err != nil {
		return wrapStore("queuing database", err)
	}
	for _, def := range defs {
		if err := batch.Set(indexDefKey(name, def.name), encodeIndexDef(def), nil); err != nil {
			return wrapStore("queuing index definition", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return wrapStore("committing database", err)
	}

	return nil
}

func (s *store) listDBs(limit int, cursor string) (names []string, listErr error) {
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: dbPrefix,
		UpperBound: prefixEnd(dbPrefix),
	})
	if err != nil {
		return nil, wrapStore("creating database iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			listErr = errors.Join(listErr, wrapStore("closing database iterator", err))
		}
	}()

	valid := iter.First()
	if cursor != "" {
		key := dbKey(cursor)
		valid = iter.SeekGE(key)
		if valid && bytes.Equal(iter.Key(), key) {
			valid = iter.Next()
		}
	}

	for ; valid && len(names) < limit; valid = iter.Next() {
		name := string(iter.Key()[len(dbPrefix):])
		if err := s.authDBRecord(iter.Key(), iter.Value()); err != nil {
			return nil, fmt.Errorf("%w: database %q", errCorruptData, name)
		}

		names = append(names, name)
	}
	if err := iter.Error(); err != nil {
		return nil, wrapStore("iterating databases", err)
	}

	return names, nil
}

func (s *store) deleteDB(name string) (deleteErr error) {
	dbMu := s.dbLock(name)
	dbMu.Lock()
	defer dbMu.Unlock()

	exists, err := s.hasDB(name)
	if err != nil {
		return fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return errDBNotFound
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			deleteErr = errors.Join(deleteErr, wrapStore("closing deletion batch", err))
		}
	}()

	if err := batch.Delete(dbKey(name), nil); err != nil {
		return wrapStore("queuing database deletion", err)
	}
	prefix := docsPrefix(name)
	if err := batch.DeleteRange(prefix, prefixEnd(prefix), nil); err != nil {
		return wrapStore("queuing document deletion", err)
	}
	prefix = indexDataPrefix(name)
	if err := batch.DeleteRange(prefix, prefixEnd(prefix), nil); err != nil {
		return wrapStore("queuing index deletion", err)
	}
	prefix = indexDefPrefix(name)
	if err := batch.DeleteRange(prefix, prefixEnd(prefix), nil); err != nil {
		return wrapStore("queuing index definition deletion", err)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return wrapStore("committing database deletion", err)
	}

	return nil
}

func (s *store) createDoc(database string, json []byte) (string, revision, error) {
	for {
		id, err := makeID()
		if err != nil {
			return "", revision{}, err
		}

		rev, err := s.createDocWithID(database, id, json)
		if errors.Is(err, errDocExists) {
			continue
		}
		if err != nil {
			return "", revision{}, err
		}

		return id, rev, nil
	}
}

func (s *store) createDocWithID(database, id string, json []byte) (rev revision, createErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	docMu := s.docLock(database, id)
	docMu.Lock()
	defer docMu.Unlock()

	databaseKey, err := s.databaseKey(database)
	if err != nil {
		return revision{}, err
	}
	defer clear(databaseKey)

	key := docKey(database, id)
	exists, err := s.has(key)
	if err != nil {
		return revision{}, fmt.Errorf("checking document: %w", err)
	}
	if exists {
		return revision{}, errDocExists
	}

	defs, err := s.indexes(database)
	if err != nil {
		return revision{}, err
	}
	values, err := indexValues(json, defs)
	if err != nil {
		return revision{}, err
	}
	rev, err = makeRevision(nil)
	if err != nil {
		return revision{}, err
	}

	record, err := sealDoc(key, databaseKey, id, rev, json)
	if err != nil {
		return revision{}, fmt.Errorf("sealing document: %w", err)
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			createErr = errors.Join(createErr, wrapStore("closing document batch", err))
		}
	}()

	if err := batch.Set(key, record, nil); err != nil {
		return revision{}, wrapStore("queuing document", err)
	}
	if err := setIndexEntries(batch, database, id, values); err != nil {
		return revision{}, err
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return revision{}, wrapStore("committing document", err)
	}

	return rev, nil
}

func (s *store) exportDocs(ctx context.Context, database string, yield func([]byte) error) (exportErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	databaseKey, err := s.databaseKey(database)
	if err != nil {
		return err
	}
	defer clear(databaseKey)

	snapshot := s.db.NewSnapshot()
	defer func() {
		if err := snapshot.Close(); err != nil {
			exportErr = errors.Join(exportErr, wrapStore("closing bulk export snapshot", err))
		}
	}()

	prefix := docsPrefix(database)
	iter, err := snapshot.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return wrapStore("creating bulk export iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			exportErr = errors.Join(exportErr, wrapStore("closing bulk export iterator", err))
		}
	}()

	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}

		id := string(iter.Key()[len(prefix):])
		if err := validateID(id); err != nil {
			return fmt.Errorf("%w: invalid document key", errCorruptData)
		}

		doc, err := openDoc(iter.Key(), databaseKey, id, iter.Value())
		if err != nil {
			return fmt.Errorf("reading export document: %w", err)
		}
		if err := yield(doc.json); err != nil {
			clear(doc.json)

			return fmt.Errorf("writing export document: %w", err)
		}
		clear(doc.json)
	}
	if err := iter.Error(); err != nil {
		return wrapStore("iterating bulk export", err)
	}

	return nil
}

func (s *store) importDocs(ctx context.Context, database string, documents [][]byte, maxBatchBytes int) (ids []string, importErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	databaseKey, err := s.databaseKey(database)
	if err != nil {
		return nil, err
	}
	defer clear(databaseKey)

	defs, err := s.indexes(database)
	if err != nil {
		return nil, err
	}

	var revisions []revision
	var unlock func()
	for {
		ids, revisions, err = makeImportIDs(len(documents))
		if err != nil {
			return nil, err
		}

		unlock = s.lockDocs(database, ids)
		collision, collisionErr := s.hasAnyDoc(database, ids)
		if collisionErr != nil {
			unlock()

			return nil, collisionErr
		}
		if !collision {
			break
		}
		unlock()
	}
	defer unlock()

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			importErr = errors.Join(importErr, wrapStore("closing bulk import batch", err))
		}
	}()

	for i, document := range documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		values, err := indexValues(document, defs)
		if err != nil {
			return nil, &bulkLineError{line: i + 1, err: err}
		}

		key := docKey(database, ids[i])
		record, err := sealDoc(key, databaseKey, ids[i], revisions[i], document)
		if err != nil {
			return nil, fmt.Errorf("sealing imported document: %w", err)
		}
		if err := batch.Set(key, record, nil); err != nil {
			return nil, wrapStore("queuing imported document", err)
		}
		if err := setIndexEntries(batch, database, ids[i], values); err != nil {
			return nil, err
		}
		if batch.Len() > maxBatchBytes {
			return nil, &bulkLineError{line: i + 1, err: errBulkLimit}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return nil, wrapStore("committing bulk import", err)
	}

	return ids, nil
}

func makeImportIDs(count int) ([]string, []revision, error) {
	ids := make([]string, 0, count)
	revisions := make([]revision, 0, count)
	seen := make(map[string]struct{}, count)

	for len(ids) < count {
		id, err := makeID()
		if err != nil {
			return nil, nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}

		rev, err := makeRevision(nil)
		if err != nil {
			return nil, nil, err
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		revisions = append(revisions, rev)
	}

	return ids, revisions, nil
}

func (s *store) hasAnyDoc(database string, ids []string) (bool, error) {
	for _, id := range ids {
		exists, err := s.has(docKey(database, id))
		if err != nil {
			return false, fmt.Errorf("checking imported document: %w", err)
		}
		if exists {
			return true, nil
		}
	}

	return false, nil
}

func (s *store) lockDocs(database string, ids []string) func() {
	var stripes [docStripeCount]bool
	for _, id := range ids {
		stripes[s.docStripe(database, id)] = true
	}
	for stripe, lock := range stripes {
		if lock {
			s.docStripes[stripe].Lock()
		}
	}

	return func() {
		for stripe := len(stripes) - 1; stripe >= 0; stripe-- {
			if stripes[stripe] {
				s.docStripes[stripe].Unlock()
			}
		}
	}
}

func (s *store) getDoc(database, id string) (storedDoc, error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	databaseKey, err := s.documentDBKey(database)
	if err != nil {
		return storedDoc{}, err
	}
	defer clear(databaseKey)

	return s.readDoc(database, id, databaseKey)
}

func (s *store) listDocs(database string, limit int, cursor string) (ids []string, more bool, listErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	exists, err := s.hasDB(database)
	if err != nil {
		return nil, false, fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return nil, false, errDBNotFound
	}

	prefix := docsPrefix(database)
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixEnd(prefix),
	})
	if err != nil {
		return nil, false, wrapStore("creating document iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			listErr = errors.Join(listErr, wrapStore("closing document iterator", err))
		}
	}()

	valid := iter.First()
	if cursor != "" {
		key := docKey(database, cursor)
		valid = iter.SeekGE(key)
		if valid && bytes.Equal(iter.Key(), key) {
			valid = iter.Next()
		}
	}

	for ; valid && len(ids) <= limit; valid = iter.Next() {
		id := string(iter.Key()[len(prefix):])
		if _, _, _, err := parseDocRecord(iter.Value()); err != nil {
			return nil, false, fmt.Errorf("%w: document %q", errCorruptData, id)
		}

		ids = append(ids, id)
	}
	if err := iter.Error(); err != nil {
		return nil, false, wrapStore("iterating documents", err)
	}
	if len(ids) > limit {
		return ids[:limit], true, nil
	}

	return ids, false, nil
}

func (s *store) replaceDoc(database, id string, json []byte, match matchCond) (rev revision, replaceErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	docMu := s.docLock(database, id)
	docMu.Lock()
	defer docMu.Unlock()

	databaseKey, err := s.documentDBKey(database)
	if err != nil {
		return revision{}, err
	}
	defer clear(databaseKey)

	key := docKey(database, id)
	current, err := s.readDoc(database, id, databaseKey)
	if err != nil {
		return revision{}, err
	}
	if !matchRevision(match, current.revision) {
		return revision{}, errPreconditionFailed
	}

	defs, err := s.indexes(database)
	if err != nil {
		return revision{}, err
	}
	oldValues, err := indexValues(current.json, defs)
	if err != nil {
		return revision{}, fmt.Errorf("%w: %v", errCorruptData, err)
	}
	newValues, err := indexValues(json, defs)
	if err != nil {
		return revision{}, err
	}
	rev, err = makeRevision(&current.revision)
	if err != nil {
		return revision{}, err
	}

	record, err := sealDoc(key, databaseKey, id, rev, json)
	if err != nil {
		return revision{}, fmt.Errorf("sealing document replacement: %w", err)
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			replaceErr = errors.Join(replaceErr, wrapStore("closing replacement batch", err))
		}
	}()

	if err := batch.Set(key, record, nil); err != nil {
		return revision{}, wrapStore("queuing document replacement", err)
	}
	if err := changeIndexEntries(batch, database, id, oldValues, newValues); err != nil {
		return revision{}, err
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return revision{}, wrapStore("committing document replacement", err)
	}

	return rev, nil
}

func (s *store) patchDoc(database, id string, match matchCond, apply func([]byte) ([]byte, error)) (doc storedDoc, patchErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	docMu := s.docLock(database, id)
	docMu.Lock()
	defer docMu.Unlock()

	databaseKey, err := s.documentDBKey(database)
	if err != nil {
		return storedDoc{}, err
	}
	defer clear(databaseKey)

	key := docKey(database, id)
	current, err := s.readDoc(database, id, databaseKey)
	if err != nil {
		return storedDoc{}, err
	}
	if !matchRevision(match, current.revision) {
		return storedDoc{}, errPreconditionFailed
	}

	json, err := apply(current.json)
	if err != nil {
		return storedDoc{}, err
	}
	defs, err := s.indexes(database)
	if err != nil {
		return storedDoc{}, err
	}
	oldValues, err := indexValues(current.json, defs)
	if err != nil {
		return storedDoc{}, fmt.Errorf("%w: %v", errCorruptData, err)
	}
	newValues, err := indexValues(json, defs)
	if err != nil {
		return storedDoc{}, err
	}
	rev, err := makeRevision(&current.revision)
	if err != nil {
		return storedDoc{}, err
	}
	doc = storedDoc{json: json, revision: rev}

	record, err := sealDoc(key, databaseKey, id, rev, json)
	if err != nil {
		return storedDoc{}, fmt.Errorf("sealing patched document: %w", err)
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			patchErr = errors.Join(patchErr, wrapStore("closing patch batch", err))
		}
	}()

	if err := batch.Set(key, record, nil); err != nil {
		return storedDoc{}, wrapStore("queuing patched document", err)
	}
	if err := changeIndexEntries(batch, database, id, oldValues, newValues); err != nil {
		return storedDoc{}, err
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return storedDoc{}, wrapStore("committing patched document", err)
	}

	return doc, nil
}

func (s *store) deleteDoc(database, id string, match matchCond) (deleteErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	docMu := s.docLock(database, id)
	docMu.Lock()
	defer docMu.Unlock()

	databaseKey, err := s.documentDBKey(database)
	if err != nil {
		return err
	}
	defer clear(databaseKey)

	key := docKey(database, id)
	current, err := s.readDoc(database, id, databaseKey)
	if err != nil {
		return err
	}
	if !matchRevision(match, current.revision) {
		return errPreconditionFailed
	}

	defs, err := s.indexes(database)
	if err != nil {
		return err
	}
	values, err := indexValues(current.json, defs)
	if err != nil {
		return fmt.Errorf("%w: %v", errCorruptData, err)
	}

	batch := s.db.NewBatch()
	defer func() {
		if err := batch.Close(); err != nil {
			deleteErr = errors.Join(deleteErr, wrapStore("closing document deletion batch", err))
		}
	}()

	if err := batch.Delete(key, nil); err != nil {
		return wrapStore("queuing document deletion", err)
	}
	if err := deleteIndexEntries(batch, database, id, values); err != nil {
		return err
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return wrapStore("committing document deletion", err)
	}

	return nil
}

func matchRevision(match matchCond, current revision) bool {
	return !match.set || match.wildcard || match.revision == current
}

func (s *store) dbLock(database string) *sync.RWMutex {
	return &s.dbStripes[hashPart(fnvOffset, database)&(dbStripeCount-1)]
}

func (s *store) docLock(database, id string) *sync.Mutex {
	return &s.docStripes[s.docStripe(database, id)]
}

func (s *store) docStripe(database, id string) int {
	hash := hashPart(fnvOffset, database)
	hash = hashPart(hashPart(hash, "\x00"), id)

	return int(hash & (docStripeCount - 1))
}

func hashPart(hash uint64, value string) uint64 {
	for i := range len(value) {
		hash ^= uint64(value[i])
		hash *= fnvPrime
	}

	return hash
}

func (s *store) hasDB(name string) (bool, error) {
	value, closer, err := s.db.Get(dbKey(name))
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, wrapStore("reading database", err)
	}
	if authErr := s.authDBRecord(dbKey(name), value); authErr != nil {
		err = fmt.Errorf("%w: database %q", errCorruptData, name)
	}
	if closeErr := closer.Close(); closeErr != nil {
		err = errors.Join(err, wrapStore("closing database value", closeErr))
	}

	return true, err
}

func (s *store) has(key []byte) (bool, error) {
	_, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, wrapStore("reading value", err)
	}
	if err := closer.Close(); err != nil {
		return false, wrapStore("closing value", err)
	}

	return true, nil
}

func (s *store) readDoc(database, id string, databaseKey []byte) (doc storedDoc, readErr error) {
	key := docKey(database, id)
	value, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return storedDoc{}, errDocNotFound
	}
	if err != nil {
		return storedDoc{}, wrapStore("reading document", err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			readErr = errors.Join(readErr, wrapStore("closing document value", err))
		}
	}()

	doc, err = openDoc(key, databaseKey, id, value)
	if err != nil {
		return storedDoc{}, fmt.Errorf("%w: document %q", errCorruptData, id)
	}

	return doc, nil
}

func (s *store) authDBRecord(pebbleKey, value []byte) error {
	key, err := unwrapDBKey(s.wrappingKey, pebbleKey, value)
	clear(key)

	return err
}

func (s *store) makeDBRecord(pebbleKey []byte) ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating database key: %w", err)
	}
	defer clear(key)

	record, err := wrapDBKey(s.wrappingKey, pebbleKey, key)
	if err != nil {
		return nil, fmt.Errorf("wrapping database key: %w", err)
	}

	return record, nil
}

func (s *store) documentDBKey(name string) ([]byte, error) {
	key, err := s.databaseKey(name)
	if errors.Is(err, errDBNotFound) {
		return nil, errDocNotFound
	}

	return key, err
}

func (s *store) databaseKey(name string) (key []byte, readErr error) {
	pebbleKey := dbKey(name)
	value, closer, err := s.db.Get(pebbleKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, errDBNotFound
	}
	if err != nil {
		return nil, wrapStore("reading database key", err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			readErr = errors.Join(readErr, wrapStore("closing database value", err))
		}
	}()

	key, err = unwrapDBKey(s.wrappingKey, pebbleKey, value)
	if err != nil {
		return nil, fmt.Errorf("%w: database %q", errCorruptData, name)
	}

	return key, nil
}

func (s *store) cursorKey(name string) ([]byte, error) {
	databaseKey, err := s.databaseKey(name)
	if err != nil {
		return nil, err
	}
	defer clear(databaseKey)

	key, err := deriveCursorKey(databaseKey)
	if err != nil {
		return nil, fmt.Errorf("deriving cursor key: %w", err)
	}

	return key, nil
}

func makeRevision(previous *revision) (revision, error) {
	for {
		var rev revision
		if _, err := rand.Read(rev[:]); err != nil {
			return revision{}, fmt.Errorf("generating revision: %w", err)
		}
		if previous == nil || rev != *previous {
			return rev, nil
		}
	}
}

func dbKey(name string) []byte {
	return appendKey(dbPrefix, name)
}

func docsPrefix(database string) []byte {
	key := appendKey(docPrefix, database)

	return append(key, 0)
}

func docKey(database, id string) []byte {
	return append(docsPrefix(database), id...)
}

func appendKey(prefix []byte, value string) []byte {
	key := make([]byte, 0, len(prefix)+len(value))
	key = append(key, prefix...)

	return append(key, value...)
}

func wrapStore(action string, err error) error {
	kind := errStoreUnavailable
	if pebble.IsCorruptionError(err) {
		kind = errCorruptData
	}

	return errors.Join(kind, fmt.Errorf("%s: %w", action, err))
}

func isStoreUnavailable(err error) bool {
	return errors.Is(err, errStoreUnavailable) || errors.Is(err, pebble.ErrClosed)
}

func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++

			return end[:i+1]
		}
	}

	return nil
}
