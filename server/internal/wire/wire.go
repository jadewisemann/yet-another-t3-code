// Package wire implements the Effect RPC JSON framing that T3 Code speaks on
// its `/ws` WebSocket: one JSON object per text frame, tagged by `_tag`.
//
// See spec/protocol/wire.md for the behavior this package follows.
package wire

import (
	"encoding/json"
	"fmt"
)

// Frame tags. Client → server: Request, Ack, Interrupt, Ping, Eof.
// Server → client: Chunk, Exit, Defect, Pong, ClientProtocolError.
const (
	TagRequest   = "Request"
	TagAck       = "Ack"
	TagInterrupt = "Interrupt"
	TagPing      = "Ping"
	TagEOF       = "Eof"
	TagChunk     = "Chunk"
	TagExit      = "Exit"
	TagDefect    = "Defect"
	TagPong      = "Pong"
)

// RequestID is a string or a number on the wire. It is kept as raw JSON so
// it is echoed back exactly as the client sent it.
type RequestID = json.RawMessage

// Frame is the union of every frame. Only the fields of its tag are set.
type Frame struct {
	Tag string `json:"_tag"`

	// Request
	ID             RequestID       `json:"id,omitempty"`
	Method         string          `json:"tag,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Headers        *[][2]string    `json:"headers,omitempty"`
	IsNotification bool            `json:"isNotification,omitempty"`
	Discard        bool            `json:"discard,omitempty"`
	TraceID        string          `json:"traceId,omitempty"`
	SpanID         string          `json:"spanId,omitempty"`
	Sampled        *bool           `json:"sampled,omitempty"`

	// Ack, Interrupt, Chunk, Exit
	RequestID RequestID `json:"requestId,omitempty"`
	// Interrupt
	Interruptors []int `json:"interruptors,omitempty"`
	// Chunk
	Values []json.RawMessage `json:"values,omitempty"`
	// Exit
	Exit *Exit `json:"exit,omitempty"`
	// Defect
	Defect json.RawMessage `json:"defect,omitempty"`
}

// Exit is the encoded Effect Exit of a request.
type Exit struct {
	Tag   string          `json:"_tag"` // "Success" or "Failure"
	Value json.RawMessage `json:"value,omitempty"`
	Cause []Cause         `json:"cause,omitempty"`
}

// Cause is one entry of a failure cause.
type Cause struct {
	Tag     string          `json:"_tag"` // "Fail", "Die", or "Interrupt"
	Error   json.RawMessage `json:"error,omitempty"`
	Defect  json.RawMessage `json:"defect,omitempty"`
	FiberID *int            `json:"fiberId,omitempty"`
}

// Success builds a successful Exit frame.
func Success(id RequestID, value any) (Frame, error) {
	raw, err := marshalValue(value)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Tag: TagExit, RequestID: id, Exit: &Exit{Tag: "Success", Value: raw}}, nil
}

// Fail builds an Exit frame that failed with a typed error (an object with `_tag`).
func Fail(id RequestID, typedError any) (Frame, error) {
	raw, err := json.Marshal(typedError)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Tag: TagExit, RequestID: id, Exit: &Exit{Tag: "Failure", Cause: []Cause{{Tag: "Fail", Error: raw}}}}, nil
}

// Die builds an Exit frame that failed with a defect.
func Die(id RequestID, message string) Frame {
	raw, _ := json.Marshal(message)
	return Frame{Tag: TagExit, RequestID: id, Exit: &Exit{Tag: "Failure", Cause: []Cause{{Tag: "Die", Defect: raw}}}}
}

// Chunk builds a stream chunk frame.
func Chunk(id RequestID, values ...any) (Frame, error) {
	if len(values) == 0 {
		return Frame{}, fmt.Errorf("a chunk needs at least one value")
	}
	out := make([]json.RawMessage, len(values))
	for i, v := range values {
		raw, err := marshalValue(v)
		if err != nil {
			return Frame{}, err
		}
		out[i] = raw
	}
	return Frame{Tag: TagChunk, RequestID: id, Values: out}, nil
}

func marshalValue(v any) (json.RawMessage, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Decode parses one text frame.
func Decode(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, err
	}
	if f.Tag == "" {
		return Frame{}, fmt.Errorf("frame has no _tag")
	}
	return f, nil
}

// IDString returns a request id as a plain string, for use as a map key.
func IDString(id RequestID) string {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}
