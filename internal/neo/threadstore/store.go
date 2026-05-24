package threadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrNotFound        = errors.New("thread not found")
	ErrVersionConflict = errors.New("thread version conflict")
)

type Store interface {
	UploadThread(ctx context.Context, thread *Thread) error
	GetThread(ctx context.Context, id string) (*Thread, error)
	ListThreads(ctx context.Context, opts ListOptions) ([]*ThreadSummary, error)
	DeleteThread(ctx context.Context, id string) error
	Close() error
}

type Thread struct {
	ID              string
	V               int
	Created         int64
	UpdatedAt       int64
	Title           string
	AgentMode       string
	ReasoningEffort string
	NextMessageID   int
	Messages        []json.RawMessage
	Meta            json.RawMessage
	Env             json.RawMessage
	CreatorUserID   string
	Raw             json.RawMessage
}

type ThreadSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	AgentMode    string `json:"agentMode"`
	Created      int64  `json:"created"`
	UpdatedAt    int64  `json:"updatedAt"`
	MessageCount int    `json:"messageCount"`
}

type ListOptions struct {
	Limit int
}

func ParseThread(raw json.RawMessage) (*Thread, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty thread json")
	}
	var data struct {
		ID              string            `json:"id"`
		V               int               `json:"v"`
		Created         int64             `json:"created"`
		UpdatedAt       int64             `json:"updatedAt"`
		Title           string            `json:"title"`
		AgentMode       string            `json:"agentMode"`
		ReasoningEffort string            `json:"reasoningEffort"`
		NextMessageID   int               `json:"nextMessageId"`
		Messages        []json.RawMessage `json:"messages"`
		Meta            json.RawMessage   `json:"meta"`
		Env             json.RawMessage   `json:"env"`
		CreatorUserID   string            `json:"creatorUserID"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.ID == "" {
		return nil, errors.New("missing thread id")
	}
	if data.V <= 0 {
		return nil, fmt.Errorf("invalid thread version %d", data.V)
	}
	return &Thread{
		ID:              data.ID,
		V:               data.V,
		Created:         data.Created,
		UpdatedAt:       data.UpdatedAt,
		Title:           data.Title,
		AgentMode:       data.AgentMode,
		ReasoningEffort: data.ReasoningEffort,
		NextMessageID:   data.NextMessageID,
		Messages:        copyRawMessages(data.Messages),
		Meta:            copyRaw(data.Meta),
		Env:             copyRaw(data.Env),
		CreatorUserID:   data.CreatorUserID,
		Raw:             copyRaw(raw),
	}, nil
}

func (t *Thread) rawJSON() (json.RawMessage, error) {
	if t == nil {
		return nil, errors.New("nil thread")
	}
	if len(t.Raw) > 0 {
		return copyRaw(t.Raw), nil
	}
	raw, err := json.Marshal(map[string]any{
		"id":              t.ID,
		"v":               t.V,
		"created":         t.Created,
		"updatedAt":       t.UpdatedAt,
		"title":           t.Title,
		"agentMode":       t.AgentMode,
		"reasoningEffort": t.ReasoningEffort,
		"nextMessageId":   t.NextMessageID,
		"messages":        t.Messages,
		"meta":            nullableRaw(t.Meta),
		"env":             nullableRaw(t.Env),
		"creatorUserID":   t.CreatorUserID,
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (t *Thread) normalized() (*Thread, error) {
	raw, err := t.rawJSON()
	if err != nil {
		return nil, err
	}
	return ParseThread(raw)
}

func copyRaw(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func copyRawMessages(in []json.RawMessage) []json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(in))
	for i := range in {
		out[i] = copyRaw(in[i])
	}
	return out
}

func nullableRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
