package main

import (
	"encoding/json"
	"fmt"

	"github.com/itchyny/gojq"
)

// applyFilter runs a compiled jq expression against body. body is expected
// to be JSON; if it doesn't parse, the filter cannot run and an error is
// returned. Output semantics:
//
//   - 1 output  → marshal that single value
//   - 0 outputs → marshal []
//   - N outputs → marshal [v1, v2, ...]
//
// Runtime errors from jq (iterator yielding a non-nil error) abort and
// return the error; callers should log and fall back to the unfiltered body.
func applyFilter(code *gojq.Code, body []byte) ([]byte, error) {
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("filter input is not JSON: %w", err)
	}

	iter := code.Run(parsed)
	var outputs []interface{}
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			return nil, fmt.Errorf("filter runtime error: %w", err)
		}
		outputs = append(outputs, v)
	}

	var marshalled []byte
	var err error
	switch len(outputs) {
	case 1:
		marshalled, err = json.Marshal(outputs[0])
	default:
		// 0 or >1: use array form to keep the shape predictable.
		if outputs == nil {
			outputs = []interface{}{}
		}
		marshalled, err = json.Marshal(outputs)
	}
	if err != nil {
		return nil, fmt.Errorf("marshal filter output: %w", err)
	}
	return marshalled, nil
}
