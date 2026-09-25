// Package jsonfile provides the single strict, durable JSON persistence path
// used by aninode's small on-disk state stores.
package jsonfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"aninode/internal/atomicfile"
)

const Mode fs.FileMode = 0o640

// Decode rejects unknown fields and trailing JSON values.
func Decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func Marshal(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func Write(path string, value any) error {
	data, err := Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, Mode)
}

func Create(path string, value any) error {
	data, err := Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Create(path, data, Mode)
}
