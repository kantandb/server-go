package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cockroachdb/pebble"
)

var (
	errIndexNotFound       = errors.New("index not found")
	errInvalidIndex        = errors.New("invalid index")
	errInvalidIndexValue   = errors.New("invalid index value")
	errUnsupportedIdxValue = errors.New("unsupported index value")
	errInvalidQueryCursor  = errors.New("invalid query cursor")
)

const (
	maxIndexes           = 16
	maxIndexPath         = 256
	maxIndexValue        = 4 << 10
	maxEncodedIndexValue = 2*maxIndexValue + 1
)

type cmpOp byte

const (
	cmpEq cmpOp = iota
	cmpLT
	cmpLE
	cmpGT
	cmpGE
)

func (op cmpOp) valid() bool {
	return op <= cmpGE
}

type indexDef struct {
	name string
	path string
}

func validateIndexes(defs []indexDef) error {
	if len(defs) > maxIndexes {
		return fmt.Errorf("%w: too many indexes", errInvalidIndex)
	}

	seen := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		if err := validateName(def.name); err != nil {
			return fmt.Errorf("%w: invalid name %q", errInvalidIndex, def.name)
		}
		if err := validatePointer(def.path); err != nil {
			return fmt.Errorf("%w: path %q: %v", errInvalidIndex, def.path, err)
		}
		if _, ok := seen[def.name]; ok {
			return fmt.Errorf("%w: duplicate name %q", errInvalidIndex, def.name)
		}
		seen[def.name] = struct{}{}
	}

	return nil
}

func validatePointer(path string) error {
	if len(path) == 0 || len(path) > maxIndexPath || path[0] != '/' || !utf8.ValidString(path) {
		return errors.New("invalid JSON Pointer")
	}

	for i := 0; i < len(path); i++ {
		if path[i] != '~' {
			continue
		}
		if i+1 == len(path) || path[i+1] != '0' && path[i+1] != '1' {
			return errors.New("invalid JSON Pointer escape")
		}
		i++
	}

	return nil
}

func indexValues(document []byte, defs []indexDef) (map[string][]byte, error) {
	var decoder objectDecoder
	root, err := decoder.decode(document, false)
	if err != nil {
		return nil, fmt.Errorf("decoding indexed document: %w", err)
	}

	values := make(map[string][]byte, len(defs))
	for _, def := range defs {
		value, ok := pointerValue(root, def.path)
		if !ok {
			continue
		}

		encoded, err := encodeIndexValue(value)
		if errors.Is(err, errUnsupportedIdxValue) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values[def.name] = encoded
	}

	return values, nil
}

func pointerValue(root map[string]any, path string) (any, bool) {
	var current any = root
	for token := range strings.SplitSeq(path[1:], "/") {
		name := strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")

		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[name]
			if !ok {
				return nil, false
			}
		case []any:
			// Array tokens use canonical decimal indices.
			index, err := strconv.Atoi(name)
			if err != nil || index < 0 || strconv.Itoa(index) != name || index >= len(value) {
				return nil, false
			}
			current = value[index]
		default:
			return nil, false
		}
	}

	return current, true
}

func encodeIndexValue(value any) ([]byte, error) {
	var encoded []byte

	switch value := value.(type) {
	case nil:
		encoded = []byte{0x00}
	case bool:
		if value {
			encoded = []byte{0x02}
		} else {
			encoded = []byte{0x01}
		}
	case json.Number:
		literal := string(value)
		if literal == "" || len(literal) >= maxIndexValue {
			return nil, fmt.Errorf("%w: invalid number", errInvalidIndexValue)
		}
		// Marshal validates JSON number syntax.
		if _, err := json.Marshal(value); err != nil {
			return nil, fmt.Errorf("%w: invalid number", errInvalidIndexValue)
		}

		if i := strings.IndexAny(literal, "eE"); i >= 0 {
			scale, err := strconv.Atoi(literal[i+1:])
			if err != nil || scale < -maxIndexValue || scale > maxIndexValue {
				return nil, fmt.Errorf("%w: number exponent is out of range", errInvalidIndexValue)
			}
		}
		number, ok := new(big.Rat).SetString(literal)
		if !ok {
			return nil, fmt.Errorf("%w: invalid number", errInvalidIndexValue)
		}
		if len(number.RatString())+1 > maxIndexValue {
			return nil, fmt.Errorf("%w: value is too large", errInvalidIndexValue)
		}
		// Keep numeric order first and use the literal spelling for equality.
		encoded = append([]byte{0x03}, encodeSortableNumber(number)...)
		encoded = append(append(encoded, literal...), 0)
	case string:
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("%w: invalid UTF-8", errInvalidIndexValue)
		}
		if len(value)+1 > maxIndexValue {
			return nil, fmt.Errorf("%w: value is too large", errInvalidIndexValue)
		}
		encoded = append([]byte{0x04}, encodeSortableString(value)...)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return encodeQueryNumber(value)
	default:
		return nil, fmt.Errorf("%w: %w", errInvalidIndexValue, errUnsupportedIdxValue)
	}

	if !validIndexValue(encoded) {
		return nil, fmt.Errorf("%w: value is too large", errInvalidIndexValue)
	}

	return encoded, nil
}

func encodeSortableNumber(number *big.Rat) []byte {
	if number.Sign() == 0 {
		return []byte{0x01}
	}

	magnitude := new(big.Rat).Abs(number)
	denominator := new(big.Int).Set(magnitude.Denom())
	twos, fives := 0, 0
	for denominator.Bit(0) == 0 {
		denominator.Rsh(denominator, 1)
		twos++
	}
	five := big.NewInt(5)
	for remainder := new(big.Int); ; fives++ {
		quotient, remainder := new(big.Int), remainder
		quotient.QuoRem(denominator, five, remainder)
		if remainder.Sign() != 0 {
			break
		}
		denominator = quotient
	}
	scale := max(twos, fives)
	coefficient := new(big.Int).Set(magnitude.Num())
	if twos < scale {
		coefficient.Mul(coefficient, new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(scale-twos)), nil))
	}
	if fives < scale {
		coefficient.Mul(coefficient, new(big.Int).Exp(five, big.NewInt(int64(scale-fives)), nil))
	}
	digits := coefficient.String()
	for digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale--
	}

	exponent := int64(len(digits) - scale)
	encoded := make([]byte, 8, 9+len(digits))
	binary.BigEndian.PutUint64(encoded, uint64(exponent)^(uint64(1)<<63))
	for i := range len(digits) {
		encoded = append(encoded, digits[i]-'0'+1)
	}
	encoded = append(encoded, 0x00)

	if number.Sign() > 0 {
		return append([]byte{0x02}, encoded...)
	}
	for i := range encoded {
		encoded[i] = ^encoded[i]
	}

	return append([]byte{0x00}, encoded...)
}

func encodeSortableString(value string) []byte {
	encoded := make([]byte, 0, len(value)+2)
	for i := range len(value) {
		if value[i] == 0x00 {
			encoded = append(encoded, 0x00, 0xff)
		} else {
			encoded = append(encoded, value[i])
		}
	}

	return append(encoded, 0x00, 0x00)
}

func encodeQueryNumber(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidIndexValue, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return nil, fmt.Errorf("%w: value must be scalar", errInvalidIndexValue)
	}

	return encodeIndexValue(number)
}

func validIndexValue(value []byte) bool {
	if len(value) == 0 || len(value) > maxEncodedIndexValue {
		return false
	}

	switch value[0] {
	case 0x00, 0x01, 0x02:
		return len(value) == 1
	case 0x03:
		end := sortableNumberEnd(value[1:])
		literal := value[1+end:]
		return end != 0 && validSortableNumber(value[1:1+end]) && len(literal) > 1 &&
			literal[len(literal)-1] == 0 && literal[len(literal)-2] >= '0' && literal[len(literal)-2] <= '9' &&
			(literal[0] == '-' || literal[0] >= '0' && literal[0] <= '9') && json.Valid(literal[:len(literal)-1])
	case 0x04:
		return validSortableString(value[1:])
	default:
		return false
	}
}

func sortableNumberEnd(value []byte) int {
	if len(value) > 0 && value[0] == 0x01 {
		return 1
	}
	if len(value) < 11 || value[0] != 0x00 && value[0] != 0x02 {
		return 0
	}
	terminator := byte(0)
	if value[0] == 0x00 {
		terminator = 0xff
	}
	position := bytes.IndexByte(value[9:], terminator)
	if position < 1 {
		return 0
	}

	return 10 + position
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

func appendPart(key []byte, part []byte) []byte {
	key = binary.AppendUvarint(key, uint64(len(part)))

	return append(key, part...)
}

func readPart(key []byte) (part, rest []byte, ok bool) {
	length, size := binary.Uvarint(key)
	if size <= 0 || length > uint64(len(key)-size) {
		return nil, nil, false
	}

	end := size + int(length)

	return key[size:end], key[end:], true
}

func indexDefPrefix(database string) []byte {
	return appendPart(append([]byte(nil), idxDefPrefix...), []byte(database))
}

func indexDefKey(database, index string) []byte {
	return appendPart(indexDefPrefix(database), []byte(index))
}

func indexDataPrefix(database string) []byte {
	return appendPart(append([]byte(nil), idxDataPrefix...), []byte(database))
}

func indexTypePrefix(database, index string, valueType byte) []byte {
	key := appendPart(indexDataPrefix(database), []byte(index))

	return append(key, valueType)
}

func indexValuePrefix(database, index string, value []byte) []byte {
	key := appendPart(indexDataPrefix(database), []byte(index))

	return append(key, value...)
}

func indexKey(database, index string, value []byte, id string) []byte {
	return append(indexValuePrefix(database, index, value), id...)
}

func encodeIndexDef(def indexDef) []byte {
	return append([]byte{indexDefVersion}, def.path...)
}

func decodeIndexDef(name string, value []byte) (indexDef, error) {
	if len(value) < 2 || value[0] != indexDefVersion {
		return indexDef{}, errors.New("invalid index definition")
	}

	def := indexDef{name: name, path: string(value[1:])}
	if err := validateIndexes([]indexDef{def}); err != nil {
		return indexDef{}, err
	}

	return def, nil
}

func (s *store) indexes(database string) (defs []indexDef, loadErr error) {
	prefix := indexDefPrefix(database)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return nil, wrapStore("creating index iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			loadErr = errors.Join(loadErr, wrapStore("closing index iterator", err))
		}
	}()

	for valid := iter.First(); valid; valid = iter.Next() {
		namePart, rest, ok := readPart(iter.Key()[len(prefix):])
		if !ok || len(rest) != 0 {
			return nil, fmt.Errorf("%w: invalid index definition key", errCorruptData)
		}

		def, err := decodeIndexDef(string(namePart), iter.Value())
		if err != nil {
			return nil, fmt.Errorf("%w: index %q: %v", errCorruptData, namePart, err)
		}
		defs = append(defs, def)
	}
	if err := iter.Error(); err != nil {
		return nil, wrapStore("iterating indexes", err)
	}
	if err := validateIndexes(defs); err != nil {
		return nil, fmt.Errorf("%w: %v", errCorruptData, err)
	}

	slices.SortFunc(defs, func(a, b indexDef) int { return bytes.Compare([]byte(a.name), []byte(b.name)) })

	return defs, nil
}

func findIndex(defs []indexDef, name string) (indexDef, bool) {
	for _, def := range defs {
		if def.name == name {
			return def, true
		}
	}

	return indexDef{}, false
}

func setIndexEntries(batch *pebble.Batch, database, id string, values map[string][]byte) error {
	for name, value := range values {
		if err := batch.Set(indexKey(database, name, value, id), nil, nil); err != nil {
			return wrapStore("queuing index entry", err)
		}
	}

	return nil
}

func changeIndexEntries(batch *pebble.Batch, database, id string, old, next map[string][]byte) error {
	for name, value := range old {
		if bytes.Equal(value, next[name]) {
			continue
		}
		if err := batch.Delete(indexKey(database, name, value, id), nil); err != nil {
			return wrapStore("queuing index deletion", err)
		}
	}

	for name, value := range next {
		if bytes.Equal(value, old[name]) {
			continue
		}
		if err := batch.Set(indexKey(database, name, value, id), nil, nil); err != nil {
			return wrapStore("queuing index entry", err)
		}
	}

	return nil
}

func deleteIndexEntries(batch *pebble.Batch, database, id string, values map[string][]byte) error {
	for name, value := range values {
		if err := batch.Delete(indexKey(database, name, value, id), nil); err != nil {
			return wrapStore("queuing index deletion", err)
		}
	}

	return nil
}

func (s *store) queryDocs(database, index string, encoded []byte, limit int, cursor string) ([]string, bool, error) {
	return s.queryDocsCtx(context.Background(), database, index, encoded, limit, cursor)
}

func (s *store) queryDocsCtx(ctx context.Context, database, index string, encoded []byte, limit int, cursor string) (ids []string, more bool, queryErr error) {
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

	defs, err := s.indexes(database)
	if err != nil {
		return nil, false, err
	}
	if _, ok := findIndex(defs, index); !ok {
		return nil, false, errIndexNotFound
	}
	if !validIndexValue(encoded) {
		return nil, false, errInvalidIndexValue
	}
	prefix := indexValuePrefix(database, index, encoded)
	snapshot := s.db.NewSnapshot()
	defer func() {
		if err := snapshot.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing query snapshot", err))
		}
	}()

	iter, err := snapshot.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return nil, false, wrapStore("creating query iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing query iterator", err))
		}
	}()

	valid := iter.First()
	if cursor != "" {
		key := append(append([]byte(nil), prefix...), cursor...)
		valid = iter.SeekGE(key)
		if valid && bytes.Equal(iter.Key(), key) {
			valid = iter.Next()
		}
	}

	for ; valid && len(ids) <= limit; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		id := string(iter.Key()[len(prefix):])
		if len(iter.Value()) != 0 || validateID(id) != nil {
			return nil, false, fmt.Errorf("%w: invalid index entry", errCorruptData)
		}

		exists, err := snapshotHas(snapshot, docKey(database, id))
		if err != nil {
			return nil, false, err
		}
		if !exists {
			return nil, false, fmt.Errorf("%w: index references missing document", errCorruptData)
		}
		ids = append(ids, id)
	}
	if err := iter.Error(); err != nil {
		return nil, false, wrapStore("iterating query", err)
	}
	if len(ids) > limit {
		return ids[:limit], true, nil
	}

	return ids, false, nil
}

type indexPage struct {
	ids       []string
	lastValue []byte
	more      bool
}

func (s *store) queryRangeDocs(database, index string, op cmpOp, encoded []byte, limit int, cursor string) ([]string, bool, error) {
	page, err := s.queryRangePage(context.Background(), database, index, op, encoded, limit, nil, cursor)

	return page.ids, page.more, err
}

func (s *store) queryRangePage(ctx context.Context, database, index string, op cmpOp, encoded []byte, limit int, afterValue []byte, afterID string) (page indexPage, queryErr error) {
	dbMu := s.dbLock(database)
	dbMu.RLock()
	defer dbMu.RUnlock()

	exists, err := s.hasDB(database)
	if err != nil {
		return indexPage{}, fmt.Errorf("checking database: %w", err)
	}
	if !exists {
		return indexPage{}, errDBNotFound
	}
	defs, err := s.indexes(database)
	if err != nil {
		return indexPage{}, err
	}
	if _, ok := findIndex(defs, index); !ok {
		return indexPage{}, errIndexNotFound
	}
	if !op.valid() || op == cmpEq || !validIndexValue(encoded) || encoded[0] != 0x03 && encoded[0] != 0x04 {
		return indexPage{}, errInvalidIndexValue
	}
	if len(afterValue) > 0 && (!validIndexValue(afterValue) || afterValue[0] != encoded[0] || validateID(afterID) != nil) {
		return indexPage{}, errInvalidQueryCursor
	}

	options, typePrefix := indexBounds(database, index, op, encoded)
	snapshot := s.db.NewSnapshot()
	defer func() {
		if err := snapshot.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing range query snapshot", err))
		}
	}()

	iter, err := snapshot.NewIter(&options)
	if err != nil {
		return indexPage{}, wrapStore("creating range query iterator", err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			queryErr = errors.Join(queryErr, wrapStore("closing range query iterator", err))
		}
	}()

	valid := iter.First()
	pastCursor := afterID == ""
	if len(afterValue) > 0 {
		key := indexKey(database, index, afterValue, afterID)
		valid = iter.SeekGE(key)
		if valid && bytes.Equal(iter.Key(), key) {
			valid = iter.Next()
		}
		pastCursor = true
	}
	var values [][]byte
	for ; valid && len(page.ids) <= limit; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return indexPage{}, err
		}

		value, id, ok := splitIndexEntry(typePrefix, iter.Key())
		if !ok || len(iter.Value()) != 0 || validateID(id) != nil {
			return indexPage{}, fmt.Errorf("%w: invalid index entry", errCorruptData)
		}
		if !pastCursor {
			pastCursor = id == afterID
			continue
		}
		exists, err := snapshotHas(snapshot, docKey(database, id))
		if err != nil {
			return indexPage{}, err
		}
		if !exists {
			return indexPage{}, fmt.Errorf("%w: index references missing document", errCorruptData)
		}
		page.ids = append(page.ids, id)
		values = append(values, value)
	}
	if err := iter.Error(); err != nil {
		return indexPage{}, wrapStore("iterating range query", err)
	}
	if len(page.ids) > limit {
		page.ids = page.ids[:limit]
		page.lastValue = values[limit-1]
		page.more = true
	}

	return page, nil
}

func indexBounds(database, index string, op cmpOp, encoded []byte) (pebble.IterOptions, []byte) {
	if op != cmpEq && encoded[0] == 0x03 {
		// All spellings of a boundary number belong to the same range.
		encoded = encoded[:1+sortableNumberEnd(encoded[1:])]
	}
	boundary := indexValuePrefix(database, index, encoded)
	if op == cmpEq {
		return pebble.IterOptions{LowerBound: boundary, UpperBound: prefixEnd(boundary)}, boundary
	}

	prefix := indexTypePrefix(database, index, encoded[0])
	options := pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)}
	switch op {
	case cmpLT:
		options.UpperBound = boundary
	case cmpLE:
		options.UpperBound = prefixEnd(boundary)
	case cmpGT:
		options.LowerBound = prefixEnd(boundary)
	case cmpGE:
		options.LowerBound = boundary
	}

	return options, prefix
}

func splitIndexEntry(prefix, key []byte) ([]byte, string, bool) {
	if !bytes.HasPrefix(key, prefix) || len(prefix) == 0 {
		return nil, "", false
	}

	rest := key[len(prefix):]
	end := 0
	switch prefix[len(prefix)-1] {
	case 0x03:
		end = sortableNumberEnd(rest)
		if end == 0 {
			return nil, "", false
		}
		position := bytes.IndexByte(rest[end:], 0)
		if position < 1 {
			return nil, "", false
		}
		end += position + 1
	case 0x04:
		for end < len(rest)-1 {
			if rest[end] != 0x00 {
				end++
				continue
			}
			if rest[end+1] == 0xff {
				end += 2
				continue
			}
			if rest[end+1] == 0x00 {
				end += 2
				break
			}
			return nil, "", false
		}
	default:
		return nil, "", false
	}
	value := append([]byte{prefix[len(prefix)-1]}, rest[:end]...)
	if !validIndexValue(value) {
		return nil, "", false
	}

	return value, string(rest[end:]), true
}

func snapshotHas(snapshot *pebble.Snapshot, key []byte) (bool, error) {
	_, closer, err := snapshot.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, wrapStore("reading query document", err)
	}
	if err := closer.Close(); err != nil {
		return false, wrapStore("closing query document", err)
	}

	return true, nil
}
