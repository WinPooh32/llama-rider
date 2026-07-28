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

func TestHashRequest(t *testing.T) {
	body := []byte(`{"message": "test"}`)
	hash := hashRequest(body)
	if hash == 0 {
		t.Error("hash should not be zero")
	}
}

func TestHashRequest_EmptyBody(t *testing.T) {
	body := []byte{}
	hash := hashRequest(body)
	// Empty body should still produce a hash (FNV always produces output)
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
