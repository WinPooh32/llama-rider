package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	if proxy == nil {
		t.Fatal("New returned nil")
	}
	if proxy.upstreamURL != "http://127.0.0.1:8081" {
		t.Errorf("unexpected upstreamURL: %q", proxy.upstreamURL)
	}
	if proxy.baseModel != "test-model" {
		t.Errorf("unexpected baseModel: %q", proxy.baseModel)
	}
	if proxy.slotSavePath != "/tmp/cache" {
		t.Errorf("unexpected slotSavePath: %q", proxy.slotSavePath)
	}
	if proxy.dumpDirectory != "/tmp/dumps" {
		t.Errorf("unexpected dumpDirectory: %q", proxy.dumpDirectory)
	}
}

func TestDetermineCacheAction_NewSystem(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, req := proxy.determineCacheAction(body)
	if req == nil {
		t.Fatal("expected parsed request")
	}
	if req.Model != "model1" {
		t.Errorf("unexpected model: %q", req.Model)
	}
	if !action.erase {
		t.Error("expected erase action for new system")
	}
	if !action.warmup {
		t.Error("expected warmup action for new system")
	}
	if action.modelCache != "model1--chat.bin" {
		t.Errorf("unexpected modelCache: %q", action.modelCache)
	}
}

func TestDetermineCacheAction_SameModel(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Compute the actual hash first
	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)

	var req requestBody
	_ = json.Unmarshal(body, &req)
	instructions := extractSystemInstructions(req.Messages)
	hash, _ := hashSystem(req.System, req.Tools, instructions)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)

	// Pre-populate system cache with the correct hash
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	proxy.modelCacheName = "model1--chat.bin"

	action, _ := proxy.determineCacheAction(body)
	if action.erase {
		t.Error("unexpected erase action")
	}
	if action.warmup {
		t.Error("unexpected warmup action")
	}
	if action.restoreChat {
		t.Error("unexpected restoreChat action")
	}
	if action.restoreSystem {
		t.Error("unexpected restoreSystem action")
	}
}

func TestDetermineCacheAction_ModelSwitch_Continuation(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Compute system cache hash programmatically
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	// Pre-populate system cache for model2 (same hash since same system prompt)
	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)
	if !action.restoreChat {
		t.Error("expected restoreChat to be true for model switch with conversation")
	}
	if !action.saveCurrent {
		t.Error("expected saveCurrent to be true for model switch")
	}
}

func TestSaveCurrentModelCache_WithCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0?action=save" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")
	proxy.modelCacheName = "model1--chat.bin"

	// Should call save endpoint
	proxy.saveCurrentModelCache()
}


func TestServeHTTP_ForwardsNonMessagesPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")

	req := httptest.NewRequest(http.MethodGet, "/v1/completions", nil)
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "upstream response" {
		t.Errorf("unexpected body: %q", w.Body.String())
	}
}

func TestServeHTTP_WithMessagesPath(t *testing.T) {
	// Mock upstream that returns a response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0?action=erase" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response": "hello"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != `{"response": "hello"}` {
		t.Errorf("unexpected body: %q", w.Body.String())
	}
}

func TestServeHTTP_WithModelSwitch(t *testing.T) {
	// Mock upstream
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0?action=save" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/slots/0?action=restore" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response": "hello"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Pre-populate system cache for model1
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)
	proxy.modelCacheName = "model1--chat.bin"

	// Switch to model2
	body := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if proxy.modelCacheName != "model2--chat.bin" {
		t.Errorf("expected modelCacheName to be updated, got %q", proxy.modelCacheName)
	}
}

func TestRestoreCache(t *testing.T) {
	var receivedURL string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")

	proxy.restoreCache("test-model--chat.bin")

	if receivedURL == "" {
		t.Error("expected URL to be set")
	}
	if !strings.Contains(receivedURL, "action=restore") {
		t.Errorf("expected URL to contain 'action=restore', got %q", receivedURL)
	}
	if !strings.Contains(receivedURL, "slots/0") {
		t.Errorf("expected URL to contain 'slots/0', got %q", receivedURL)
	}
	if receivedBody == "" {
		t.Error("expected body to be set")
	}
	if !strings.Contains(receivedBody, "test-model--chat.bin") {
		t.Errorf("expected body to contain filename, got %q", receivedBody)
	}
}

func TestRestoreCache_WithColons(t *testing.T) {
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")

	// Test that colons are escaped
	proxy.restoreCache("model:name--chat.bin")

	if receivedBody == "" {
		t.Error("expected body to be set")
	}
	// The body should contain the escaped filename (colons replaced with --)
	if !strings.Contains(receivedBody, "model--name--chat.bin") {
		t.Errorf("expected escaped filename 'model--name--chat.bin' in body, got %q", receivedBody)
	}
}

func TestEraseCache(t *testing.T) {
	var receivedURL string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")

	proxy.eraseCache()

	if receivedURL == "" {
		t.Error("expected URL to be set")
	}
	if !strings.Contains(receivedURL, "action=erase") {
		t.Errorf("expected URL to contain 'action=erase', got %q", receivedURL)
	}
	if !strings.Contains(receivedURL, "slots/0") {
		t.Errorf("expected URL to contain 'slots/0', got %q", receivedURL)
	}
}

func TestSaveCache(t *testing.T) {
	var receivedURL string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")

	proxy.saveCache("test-model--chat.bin")

	if receivedURL == "" {
		t.Error("expected URL to be set")
	}
	if !strings.Contains(receivedURL, "action=save") {
		t.Errorf("expected URL to contain 'action=save', got %q", receivedURL)
	}
	if !strings.Contains(receivedURL, "slots/0") {
		t.Errorf("expected URL to contain 'slots/0', got %q", receivedURL)
	}
	if receivedBody == "" {
		t.Error("expected body to be set")
	}
	if !strings.Contains(receivedBody, "test-model--chat.bin") {
		t.Errorf("expected body to contain filename, got %q", receivedBody)
	}
}

func TestDetermineCacheAction_NoSaveOnSameModel(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Pre-populate system cache
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	// Set current model cache
	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

	// Should NOT save current model cache when same model, same system prompt
	if action.saveCurrent {
		t.Error("expected saveCurrent to be false for same model/system")
	}
	if action.erase {
		t.Error("expected erase to be false for same model/system")
	}
	if action.warmup {
		t.Error("expected warmup to be false for same model/system")
	}
	if action.restoreChat {
		t.Error("expected restoreChat to be false for same model/system")
	}
	if action.restoreSystem {
		t.Error("expected restoreSystem to be false for same model/system")
	}
}

func TestDetermineCacheAction_SaveOnModelSwitch(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Compute system cache hash programmatically
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	// Pre-populate system cache for model2 (same hash since same system prompt)
	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	// Set current model cache
	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

	// Should save current model cache when switching model
	if !action.saveCurrent {
		t.Error("expected saveCurrent to be true for model switch")
	}
	if !action.restoreChat {
		t.Error("expected restoreChat to be true for model switch with conversation")
	}
}

func TestDetermineCacheAction_SaveOnNewSystem(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Set current model cache
	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "new system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

	// Should save current model cache when new system prompt
	if !action.saveCurrent {
		t.Error("expected saveCurrent to be true for new system prompt")
	}
	if !action.erase {
		t.Error("expected erase to be true for new system prompt")
	}
	if !action.warmup {
		t.Error("expected warmup to be true for new system prompt")
	}
}

func TestDetermineCacheAction_ModelSwitch_EmptyConversation(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Compute system cache hash programmatically
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	// Pre-populate system cache for model2 (same hash since same system prompt)
	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	// Set current model cache
	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

	// Should save current model cache when switching model
	if !action.saveCurrent {
		t.Error("expected saveCurrent to be true for model switch")
	}
	// Should restore system cache (not chat cache) for user-only conversation
	if !action.restoreSystem {
		t.Error("expected restoreSystem to be true for model switch with user-only conversation")
	}
	if action.restoreChat {
		t.Error("expected restoreChat to be false for user-only conversation")
	}
	if action.erase {
		t.Error("expected erase to be false (system cache exists)")
	}
	if action.warmup {
		t.Error("expected warmup to be false (system cache exists)")
	}
}

func TestServeHTTP_WithEmptyBaseModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "", "/tmp/cache", "/tmp/dumps")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestSaveChatCache_WhenLocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0?action=save" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")
	proxy.modelCacheName = "model1--chat.bin"

	// Hold the lock briefly in a goroutine, then release it.
	// SaveChatCache should acquire the lock after the goroutine releases it.
	released := make(chan struct{})
	go func() {
		proxy.mu.Lock()
		close(released)
		proxy.mu.Unlock()
	}()

	<-released // Wait for goroutine to release the lock

	// Now call SaveChatCache; it should acquire the lock and save successfully
	proxy.SaveChatCache()
}

func TestSaveChatCache_WhenUnlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0?action=save" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "model1", "/tmp/cache", "/tmp/dumps")
	proxy.modelCacheName = "model1--chat.bin"

	// Should save cache
	proxy.SaveChatCache()
}

func TestWarmupSystem_EmptyContents(t *testing.T) {
	var receivedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	// Call warmupSystem with nil contents (should create empty text message)
	proxy.warmupSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)

	if receivedBody == nil {
		t.Fatal("expected request body to be received")
	}

	if receivedBody["model"] != "test-model" {
		t.Errorf("expected model 'test-model', got %v", receivedBody["model"])
	}

	if receivedBody["max_tokens"] != float64(0) {
		t.Errorf("expected max_tokens 0, got %v", receivedBody["max_tokens"])
	}

	if receivedBody["stream"] != false {
		t.Errorf("expected stream false, got %v", receivedBody["stream"])
	}

	messages, ok := receivedBody["messages"].([]any)
	if !ok {
		t.Fatal("expected messages to be an array")
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	msg, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatal("expected message to be an object")
	}
	if msg["role"] != "user" {
		t.Errorf("expected role 'user', got %v", msg["role"])
	}

	contents, ok := msg["content"].([]any)
	if !ok {
		t.Fatal("expected content to be an array")
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(contents))
	}

	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatal("expected content item to be an object")
	}
	if content["type"] != "text" {
		t.Errorf("expected content type 'text', got %v", content["type"])
	}
	if content["text"] != "" {
		t.Errorf("expected empty text, got %v", content["text"])
	}
}

func TestWarmupSystem_WithContents(t *testing.T) {
	var receivedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	contents := []Content{
		{Type: "text", Text: "instruction 1"},
		{Type: "text", Text: "instruction 2"},
	}
	proxy.warmupSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), contents)

	if receivedBody == nil {
		t.Fatal("expected request body to be received")
	}

	messages, ok := receivedBody["messages"].([]any)
	if !ok {
		t.Fatal("expected messages to be an array")
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	msg, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatal("expected message to be an object")
	}

	rc, ok := msg["content"].([]any)
	if !ok {
		t.Fatal("expected content to be an array")
	}
	if len(rc) != 2 {
		t.Fatalf("expected 2 content items, got %d", len(rc))
	}
}

func TestWarmupSystem_WithTools(t *testing.T) {
	var receivedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	tools := json.RawMessage(`[{"name":"test_tool"}]`)
	proxy.warmupSystem(json.RawMessage(`"system prompt"`), tools, nil)

	if receivedBody == nil {
		t.Fatal("expected request body to be received")
	}

	_, hasTools := receivedBody["tools"]
	if !hasTools {
		t.Error("expected tools to be present in request body")
	}
}

func TestWarmupSystem_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	// Should not panic or error - just log a warning
	proxy.warmupSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
}
