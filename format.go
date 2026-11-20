package main

const (
	storeMetaVersion byte = 1
	dbRecordVersion  byte = 1
	docRecordVersion byte = 1
	indexDefVersion  byte = 2

	cipherSuite1 byte = 1
	wrappingKey1 byte = 1
)

const (
	keySize      = 32
	storeSaltLen = 32
	gcmNonceLen  = 12
	gcmTagLen    = 16

	storeMetaHeaderLen = 1 + 1 + storeSaltLen + gcmNonceLen
	dbRecordHeaderLen  = 1 + 1 + gcmNonceLen
	docRecordHeaderLen = 1 + 1 + 16 + 8 + gcmNonceLen
)
