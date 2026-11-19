package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/valyala/fastjson"
)

var errJSONObject = errors.New("value must be a JSON object")

type objectDecoder struct {
	parser fastjson.Parser
}

func (d *objectDecoder) decode(body []byte, stored bool) (map[string]any, error) {
	if node := d.parse(body, stored); node != nil && node.Type() == fastjson.TypeObject {
		return jsonObject(node, nil), nil
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errJSONObject
	}
	if err := jsonEnd(decoder); err != nil {
		return nil, err
	}

	return object, nil
}

func (d *objectDecoder) parse(body []byte, stored bool) *fastjson.Value {
	// Only stored, canonical JSON can skip syntax and escape checks.
	if !stored && (bytes.Contains(body, []byte(`\u`)) || !json.Valid(body)) {
		return nil
	}
	node, err := d.parser.ParseBytes(body)
	if err != nil {
		return nil
	}

	return node
}

func jsonObject(node *fastjson.Value, duplicate *bool) map[string]any {
	obj := node.GetObject()
	out := make(map[string]any, obj.Len())
	obj.Visit(func(key []byte, value *fastjson.Value) {
		name := string(key)
		if duplicate != nil {
			if _, ok := out[name]; ok {
				*duplicate = true
			}
		}
		out[name] = jsonValue(value, duplicate)
	})

	return out
}

func jsonValue(node *fastjson.Value, duplicate *bool) any {
	// Copy values that would otherwise refer to the reusable parser buffer.
	switch node.Type() {
	case fastjson.TypeObject:
		return jsonObject(node, duplicate)
	case fastjson.TypeArray:
		values := node.GetArray()
		out := make([]any, len(values))
		for i, value := range values {
			out[i] = jsonValue(value, duplicate)
		}

		return out
	case fastjson.TypeString:
		return string(node.GetStringBytes())
	case fastjson.TypeNumber:
		var scratch [32]byte

		return json.Number(node.MarshalTo(scratch[:0]))
	case fastjson.TypeTrue:
		return true
	case fastjson.TypeFalse:
		return false
	default:
		return nil
	}
}

func jsonEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return err
	}

	return nil
}
