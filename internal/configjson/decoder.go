// Package configjson decodes strict typed operator configuration documents.
package configjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

// Decode rejects nulls, duplicate keys, unknown fields and incorrect casing.
func Decode(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := uniqueJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("configuration document has trailing data")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("configuration document is invalid JSON")
	}
	if !exactFields(value, reflect.TypeOf(destination).Elem()) {
		return errors.New("configuration document contains unknown or incorrectly cased fields")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return errors.New("configuration document has invalid field types")
	}
	return nil
}

func uniqueJSON(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("configuration nesting exceeds bounds")
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return errors.New("configuration document contains invalid JSON or null")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			key, keyErr := decoder.Token()
			name, valid := key.(string)
			if keyErr != nil || !valid || seen[name] {
				return errors.New("configuration document contains duplicate keys")
			}
			seen[name] = true
		}
		if childErr := uniqueJSON(decoder, depth+1); childErr != nil {
			return childErr
		}
	}
	if _, err = decoder.Token(); err != nil {
		return errors.New("configuration document is incomplete")
	}
	return nil
}

func exactFields(value any, kind reflect.Type) bool {
	if kind.Kind() == reflect.Pointer {
		return exactFields(value, kind.Elem())
	}
	switch typed := value.(type) {
	case map[string]any:
		if kind.Kind() != reflect.Struct {
			return false
		}
		fields := map[string]reflect.Type{}
		for index := 0; index < kind.NumField(); index++ {
			field := kind.Field(index)
			fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		for name, child := range typed {
			field, found := fields[name]
			if !found || !exactFields(child, field) {
				return false
			}
		}
	case []any:
		if kind.Kind() != reflect.Slice {
			return false
		}
		for _, child := range typed {
			if !exactFields(child, kind.Elem()) {
				return false
			}
		}
	}
	return true
}
