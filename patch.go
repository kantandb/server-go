package main

import (
	"errors"
	"fmt"
	"mime"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

const (
	mergePatchType = "application/merge-patch+json"
	jsonPatchType  = "application/json-patch+json"
)

var errInvalidPatch = errors.New("invalid patch")

func parsePatchType(value string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || mediaType != mergePatchType && mediaType != jsonPatchType {
		return "", errInvalidPatch
	}

	return mediaType, nil
}

func applyPatch(document, body []byte, mediaType string, maxBytes int64) ([]byte, error) {
	var result []byte
	var err error

	switch mediaType {
	case mergePatchType:
		result, err = jsonpatch.MergePatch(document, body)
	case jsonPatchType:
		var patch jsonpatch.Patch
		patch, err = jsonpatch.DecodePatch(body)
		if err == nil {
			options := jsonpatch.NewApplyOptions()
			options.SupportNegativeIndices = false
			options.AccumulatedCopySizeLimit = maxBytes
			result, err = patch.ApplyWithOptions(document, options)
		}
	default:
		return nil, errInvalidPatch
	}
	if err != nil {
		if _, ok := errors.AsType[*jsonpatch.AccumulatedCopySizeError](err); ok {
			return nil, errBodyTooLarge
		}

		return nil, fmt.Errorf("%w: %v", errInvalidPatch, err)
	}

	result, err = validateDoc(result, maxBytes)
	if errors.Is(err, errBodyTooLarge) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: result must be a JSON object", errInvalidPatch)
	}

	return result, nil
}
