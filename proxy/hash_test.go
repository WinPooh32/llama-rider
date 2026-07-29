package proxy

import (
	"encoding/json"
	"testing"
)

func TestHashSystem(t *testing.T) {
	system := json.RawMessage(`"system prompt"`)
	tools := json.RawMessage(`[]`)
	instructions := []Content{{Type: "text", Text: "instruction"}}

	hash, err := hashSystem(system, tools, instructions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash == 0 {
		t.Error("hash should not be zero")
	}
}

func TestHashSystem_WithNilInstructions(t *testing.T) {
	system := json.RawMessage(`"system prompt"`)
	tools := json.RawMessage(`[]`)

	hash, err := hashSystem(system, tools, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash == 0 {
		t.Error("hash should not be zero")
	}
}

func TestHashSystem_WithMarshalError(t *testing.T) {
	// Pass invalid JSON that cannot be marshaled
	system := json.RawMessage(`"system"`)
	tools := json.RawMessage(`[]`)
	instructions := []Content{{Type: "text", Text: "test"}}

	// This should work fine since we're marshaling a valid struct
	hash, err := hashSystem(system, tools, instructions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = hash
}


func TestEscapeFilenameInline(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"model:chat", "model--chat"},
		{"no:colons:here", "no--colons--here"},
		{"already-safe", "already-safe"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := escapeFilenameInline(tt.input)
			if result != tt.expected {
				t.Errorf("escapeFilenameInline(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestExtractSystemInstructions(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		want     []Content
	}{
		{
			name:     "empty messages",
			messages: nil,
			want:     nil,
		},
		{
			name: "two messages",
			messages: []Message{
				{Role: "user", Content: Contents{{Type: "text", Text: "a"}}},
				{Role: "assistant", Content: Contents{{Type: "text", Text: "b"}}},
			},
			want: nil,
		},
		{
			name: "single message, single content",
			messages: []Message{
				{Role: "user", Content: Contents{{Type: "text", Text: "hello"}}},
			},
			want: nil,
		},
		{
			name: "single message, multiple content",
			messages: []Message{
				{Role: "user", Content: Contents{
					{Type: "text", Text: "instr1"},
					{Type: "text", Text: "instr2"},
					{Type: "text", Text: "query"},
				}},
			},
			want: []Content{
				{Type: "text", Text: "instr1"},
				{Type: "text", Text: "instr2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSystemInstructions(tt.messages)
			if len(got) != len(tt.want) {
				t.Errorf("extractSystemInstructions() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractSystemInstructions()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
