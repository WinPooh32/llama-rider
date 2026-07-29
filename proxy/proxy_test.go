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
	"sync"
	"testing"
)

func TestNew_ForwardsRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	proxy := New(upstream, "test-model", "/tmp/cache", "/tmp/dumps")

	if proxy == nil {
		t.Fatal("New returned nil")
	}

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

	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=erase" {
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
	var saveReceived bool
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=save" {
			saveReceived = true
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// First request to establish model1 cache
	firstBody := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Switch to model2 - should trigger save of model1 cache
	secondBody := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	w2 := httptest.NewRecorder()
	proxy.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w2.Code)
	}
	if !saveReceived {
		t.Error("expected save to be called on model switch")
	}
}

func TestServeHTTP_TriggersRestoreOnModelSwitch(t *testing.T) {
	var restoreBody string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=restore" {
			mu.Lock()
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			restoreBody = string(buf[:n])
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Pre-create system cache for model2 so switch triggers restoreChat instead of erase+warmup
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	// First request to establish model1 cache
	firstBody := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Switch to model2 with conversation continuation
	secondBody := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	w2 := httptest.NewRecorder()
	proxy.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w2.Code)
	}
	if !strings.Contains(restoreBody, "model2--chat.bin") {
		t.Errorf("expected restore with model2 cache, got %q", restoreBody)
	}
}

func TestServeHTTP_TriggersRestoreWithEscapedFilename(t *testing.T) {
	var restoreBody string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=restore" {
			mu.Lock()
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			restoreBody = string(buf[:n])
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model:name", tmpDir, "/tmp/dumps")

	// First request to establish cache with colons in model name
	firstBody := []byte(`{
		"model": "model:name",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Switch to another model with colons
	secondBody := []byte(`{
		"model": "other:model",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	w2 := httptest.NewRecorder()
	proxy.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w2.Code)
	}
	// The filename should have colons escaped
	if strings.Contains(restoreBody, ":") {
		t.Errorf("expected colons to be escaped in restore filename, got %q", restoreBody)
	}
}

func TestServeHTTP_TriggersEraseOnNewSystem(t *testing.T) {
	var eraseReceived bool
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=erase" {
			eraseReceived = true
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "new system prompt",
		"tools": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if !eraseReceived {
		t.Error("expected erase to be called for new system prompt")
	}
}

func TestServeHTTP_TriggersSaveOnModelSwitch(t *testing.T) {
	var saveReceived bool
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=save" {
			saveReceived = true
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Pre-create system cache for model2 so switch doesn't trigger erase/warmup
	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	// First request to establish cache
	firstBody := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Switch model - should trigger save of current cache before switch
	secondBody := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	w2 := httptest.NewRecorder()
	proxy.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w2.Code)
	}
	if !saveReceived {
		t.Error("expected save to be called on model switch")
	}
}

func TestDetermineCacheAction_NoSaveOnSameModel(t *testing.T) {
	tmpDir := t.TempDir()
	upstream, _ := url.Parse("http://127.0.0.1:8081")
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

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

	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

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

	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "new system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

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

	hash, _ := hashSystem(json.RawMessage(`"system prompt"`), json.RawMessage(`[]`), nil)
	systemCache := fmt.Sprintf("model1%s%x.bin", systemCacheSeparator, hash)
	cacheFile := filepath.Join(tmpDir, systemCache)
	_ = os.WriteFile(cacheFile, []byte("test"), 0644)

	systemCache2 := fmt.Sprintf("model2%s%x.bin", systemCacheSeparator, hash)
	cacheFile2 := filepath.Join(tmpDir, systemCache2)
	_ = os.WriteFile(cacheFile2, []byte("test"), 0644)

	proxy.modelCacheName = "model1--chat.bin"

	body := []byte(`{
		"model": "model2",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	action, _ := proxy.determineCacheAction(body)

	if !action.saveCurrent {
		t.Error("expected saveCurrent to be true for model switch")
	}
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

func TestSaveChatCache_Concurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Establish cache through ServeHTTP first
	firstBody := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Now test concurrent SaveChatCache
	var wg sync.WaitGroup
	done := make(chan bool, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		proxy.SaveChatCache()
		done <- true
	}()

	proxy.SaveChatCache()

	<-done
	wg.Wait()
}

func TestSaveChatCache_SavesCache(t *testing.T) {
	var saveReceived bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots/0" && r.URL.RawQuery == "action=save" {
			saveReceived = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "model1", tmpDir, "/tmp/dumps")

	// Establish cache through ServeHTTP
	firstBody := []byte(`{
		"model": "model1",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	w1 := httptest.NewRecorder()
	proxy.ServeHTTP(w1, req1)

	// Now call SaveChatCache
	proxy.SaveChatCache()

	if !saveReceived {
		t.Error("expected SaveChatCache to trigger save")
	}
}

func TestServeHTTP_TriggersWarmupWithEmptyContents(t *testing.T) {
	var warmupBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && !strings.Contains(r.URL.RawQuery, "action") {
			_ = json.NewDecoder(r.Body).Decode(&warmupBody)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "test-model", tmpDir, "/tmp/dumps")

	// Send request that triggers warmup (no system cache exists)
	body := []byte(`{
		"model": "test-model",
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
	if warmupBody == nil {
		t.Fatal("expected warmup request to be received")
	}
	if warmupBody["model"] != "test-model" {
		t.Errorf("expected model 'test-model', got %v", warmupBody["model"])
	}
}

func TestServeHTTP_TriggersWarmupWithContents(t *testing.T) {
	var warmupBody map[string]any
	var decoded bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && r.URL.RawQuery == "" && !decoded {
			decoded = true
			_ = json.NewDecoder(r.Body).Decode(&warmupBody)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "test-model", tmpDir, "/tmp/dumps")

	// Send request with multiple content blocks (instructions = all but last)
	body := []byte(`{
		"model": "test-model",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "instruction 1"}, {"type": "text", "text": "instruction 2"}, {"type": "text", "text": "last"}]}],
		"system": "system prompt",
		"tools": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if warmupBody == nil {
		t.Fatal("expected warmup request to be received")
	}

	messages, ok := warmupBody["messages"].([]any)
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
	// extractSystemInstructions returns all but last content = 2 items
	if len(rc) != 2 {
		t.Fatalf("expected 2 content items (instructions exclude last), got %d", len(rc))
	}
}

func TestServeHTTP_TriggersWarmupWithTools(t *testing.T) {
	var warmupBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && !strings.Contains(r.URL.RawQuery, "action") {
			_ = json.NewDecoder(r.Body).Decode(&warmupBody)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "test-model", tmpDir, "/tmp/dumps")

	tools := json.RawMessage(`[{"name":"test_tool"}]`)
	body := fmt.Sprintf(`{
		"model": "test-model",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": %s
	}`, tools)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if warmupBody == nil {
		t.Fatal("expected warmup request to be received")
	}

	_, hasTools := warmupBody["tools"]
	if !hasTools {
		t.Error("expected tools to be present in warmup request")
	}
}

func TestServeHTTP_WarmupHTTPErrorDoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	upstream, _ := url.Parse(server.URL)
	tmpDir := t.TempDir()
	proxy := New(upstream, "test-model", tmpDir, "/tmp/dumps")

	body := []byte(`{
		"model": "test-model",
		"messages": [{"role": "user", "content": "hello"}],
		"system": "system prompt",
		"tools": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	// Should not panic
	proxy.ServeHTTP(w, req)
}
