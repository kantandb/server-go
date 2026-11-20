package main

import "strconv"

type queryDocument struct {
	id string
	storedDoc
}

func (doc *queryDocument) wrap(value []byte) []byte {
	body := make([]byte, 0, len(value)+128)
	body = append(body, `{"id":`...)
	body = strconv.AppendQuote(body, doc.id)
	body = append(body, `,"rev":`...)
	body = strconv.AppendQuote(body, formatETag(doc.revision))
	body = append(body, `,"doc":`...)
	body = append(body, value...)

	return append(body, '}')
}
