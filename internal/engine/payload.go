package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const payloadVersion = 1

var errPayload = errors.New("engine: the inbox payload is not one event of a known version")

type envelope struct {
	V       int      `json:"v"`
	Message *Message `json:"message,omitempty"`
	Group   *Group   `json:"group,omitempty"`
}

func encodePayload(ev Event) ([]byte, error) {
	env := envelope{V: payloadVersion}
	switch e := ev.(type) {
	case Message:
		env.Message = &e
	case Group:
		env.Group = &e
	default:
		return nil, errPayload
	}
	return json.Marshal(env)
}

func decodePayload(b []byte) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, errPayload
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) || env.V != payloadVersion {
		return nil, errPayload
	}
	switch {
	case env.Message != nil && env.Group == nil:
		return *env.Message, nil
	case env.Group != nil && env.Message == nil:
		return *env.Group, nil
	}
	return nil, errPayload
}
