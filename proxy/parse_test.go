package proxy

import (
	"encoding/json"
	"testing"
)

func TestContents_UnmarshalJSON_Null(t *testing.T) {
	var c Contents
	err := json.Unmarshal([]byte("null"), &c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c) != 0 {
		t.Errorf("expected empty Contents, got %v", c)
	}
}

func TestContents_UnmarshalJSON_String(t *testing.T) {
	var c Contents
	err := json.Unmarshal([]byte(`"hello"`), &c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c) != 1 {
		t.Fatalf("expected 1 content, got %d", len(c))
	}
	if c[0].Type != "text" || c[0].Text != "hello" {
		t.Errorf("unexpected content: %+v", c[0])
	}
}

func TestContents_UnmarshalJSON_Array(t *testing.T) {
	var c Contents
	err := json.Unmarshal([]byte(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`), &c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(c))
	}
	if c[0].Text != "first" || c[1].Text != "second" {
		t.Errorf("unexpected contents: %+v", c)
	}
}

func TestIsUserOnlyConversation_AllUser(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: Contents{{Type: "text", Text: "hi"}}},
		{Role: "user", Content: Contents{{Type: "text", Text: "hello"}}},
	}
	if !isUserOnlyConversation(messages) {
		t.Error("expected all user messages")
	}
}

func TestIsUserOnlyConversation_Mixed(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: Contents{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: Contents{{Type: "text", Text: "hello"}}},
	}
	if isUserOnlyConversation(messages) {
		t.Error("expected mixed messages, not all user")
	}
}

func TestIsUserOnlyConversation_Empty(t *testing.T) {
	messages := []Message{}
	if !isUserOnlyConversation(messages) {
		t.Error("expected empty conversation to be user-only")
	}
}

func TestExtractSystemInstructions_SingleMessage(t *testing.T) {
	messages := []Message{
		{
			Role: "user",
			Content: Contents{
				{Type: "text", Text: "system instruction"},
				{Type: "text", Text: "user message"},
			},
		},
	}
	result := extractSystemInstructions(messages)
	if len(result) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(result))
	}
	if result[0].Text != "system instruction" {
		t.Errorf("unexpected instruction text: %q", result[0].Text)
	}
}

func TestExtractSystemInstructions_SingleContent(t *testing.T) {
	messages := []Message{
		{
			Role: "user",
			Content: Contents{
				{Type: "text", Text: "just one message"},
			},
		},
	}
	result := extractSystemInstructions(messages)
	if result != nil {
		t.Errorf("expected nil for single content, got %v", result)
	}
}

func TestExtractSystemInstructions_MultipleMessages(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: Contents{{Type: "text", Text: "first"}}},
		{Role: "assistant", Content: Contents{{Type: "text", Text: "second"}}},
	}
	result := extractSystemInstructions(messages)
	if result != nil {
		t.Errorf("expected nil for multiple messages, got %v", result)
	}
}

func TestExtractSystemInstructions_Empty(t *testing.T) {
	messages := []Message{}
	result := extractSystemInstructions(messages)
	if result != nil {
		t.Errorf("expected nil for empty messages, got %v", result)
	}
}
