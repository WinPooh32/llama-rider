package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	messagesPath         = "/v1/messages"
	slotPath             = "/slots/0"
	jsonContentType      = "application/json"
	chatCacheSuffix      = "--chat.bin"
	systemCacheSeparator = "--system--"

	maxSaveRetries = 3

	defaultClientTimeout = 120 * time.Second
)

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Contents []Content

func (c *Contents) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}

	if bytes.HasPrefix(data, []byte("\"")) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}

		*c = Contents{{
			Type: "text",
			Text: s,
		}}

		return nil
	}

	var cc []Content
	if err := json.Unmarshal(data, &cc); err != nil {
		return err
	}

	*c = cc

	return nil
}

type Message struct {
	Role    string   `json:"role"`
	Content Contents `json:"content"`
}

// Proxy is an HTTP reverse proxy that intercepts llama-server requests
// to manage KV-cache disk offload. It saves and restores model state
// to/from the filesystem based on model switches and conversation context.
type Proxy struct {
	reverseProxy     *httputil.ReverseProxy
	upstreamURL      string
	baseModel        string
	slotSavePath     string
	dumpDirectory    string
	systemCacheLimit int
	client           *http.Client
	mu               sync.Mutex
	modelCacheName   string
}

// New creates a new Proxy that forwards requests to the given upstream URL
// and manages KV-cache files in slotSavePath.
func New(upstream *url.URL, model, slotSavePath, dumpDir string, systemCacheLimit int) *Proxy {
	return &Proxy{
		reverseProxy:     httputil.NewSingleHostReverseProxy(upstream),
		upstreamURL:      upstream.String(),
		baseModel:        model,
		slotSavePath:     slotSavePath,
		dumpDirectory:    dumpDir,
		systemCacheLimit: systemCacheLimit,
		client: &http.Client{
			Timeout: defaultClientTimeout,
		},
	}
}

type requestBody struct {
	System    json.RawMessage `json:"system"`
	Tools     json.RawMessage `json:"tools"`
	Messages  []Message       `json:"messages"`
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
}

// ServeHTTP implements http.Handler. It intercepts llama-server requests
// to manage KV-cache state: saving before model switches, restoring
// previously-warmed caches, and forwarding requests to the upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	slog.Info("proxy", "method", r.Method, "path", r.URL.Path, "upstream", p.upstreamURL)

	if p.baseModel == "" || r.URL.Path != messagesPath {
		p.reverseProxy.ServeHTTP(w, r)
		return
	}

	body, err := p.readRequestBody(r)
	if err != nil {
		slog.Error("read body", "err", err)
		p.reverseProxy.ServeHTTP(w, r)
		return
	}

	action, req := p.determineCacheAction(body)
	p.executeCacheAction(action, req)

	r.Body = io.NopCloser(bytes.NewReader(body))
	p.reverseProxy.ServeHTTP(w, r)

	p.modelCacheName = action.modelCache
}

// SaveChatCache saves the current model's chat cache.
func (p *Proxy) SaveChatCache() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.modelCacheName == "" {
		return
	}

	p.saveCache(p.modelCacheName)
}

// readRequestBody reads the request body and returns it as bytes.
func (p *Proxy) readRequestBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// cacheAction describes what cache operation to perform for a request.
type cacheAction struct {
	modelCache    string
	systemCache   string
	erase         bool
	warmup        bool
	restoreChat   bool
	restoreSystem bool
	saveCurrent   bool
}

// determineCacheAction reads the request body, computes cache keys,
// and returns the appropriate cache action along with the parsed request.
func (p *Proxy) determineCacheAction(body []byte) (cacheAction, *requestBody) {
	var req requestBody
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Error("parse body", "err", err)
		return cacheAction{}, nil
	}

	instructions := extractSystemInstructions(req.Messages)

	modelCache := req.Model + chatCacheSuffix
	systemHash, err := hashSystem(req.System, req.Tools, instructions)
	if err != nil {
		slog.Warn("hash system", "err", err)
		return cacheAction{}, &req
	}
	systemCache := fmt.Sprintf("%s%s%x.bin", req.Model, systemCacheSeparator, systemHash)
	switchedModel := p.modelCacheName != modelCache

	action := cacheAction{
		modelCache:  modelCache,
		systemCache: systemCache,
	}

	if !systemCacheFileExists(p.slotSavePath, systemCache) {
		action.erase = true
		action.warmup = true
		action.saveCurrent = true // Save before erasing new system prompt
	} else if switchedModel && !isUserOnlyConversation(req.Messages) {
		action.restoreChat = true
		action.saveCurrent = true // Save before switching model
	} else if switchedModel {
		action.restoreSystem = true
		action.saveCurrent = true // Save before switching model
	}

	return action, &req
}

// executeCacheAction performs the save/erase/restore sequence described
// by the given cache action.
func (p *Proxy) executeCacheAction(action cacheAction, req *requestBody) {
	if action.modelCache == "" {
		return
	}

	if action.saveCurrent {
		p.saveCurrentModelCache()
	}

	if action.erase {
		p.eraseCache()
	}

	if action.warmup && req != nil {
		instructions := extractSystemInstructions(req.Messages)
		p.warmupSystem(req.System, req.Tools, instructions)
		p.saveCache(action.systemCache)
		p.cleanupSystemCaches(req.Model, action.systemCache)
	}

	if action.restoreChat {
		p.restoreCache(action.modelCache)
	}

	if action.restoreSystem {
		p.restoreCache(action.systemCache)
	}
}

// saveCurrentModelCache saves the current model's cache if one exists.
func (p *Proxy) saveCurrentModelCache() {
	if p.modelCacheName == "" {
		return
	}
	p.saveCache(p.modelCacheName)
}

func (p *Proxy) warmupSystem(system json.RawMessage, tools json.RawMessage, contents []Content) {
	slog.Info("warmup system", "model", p.baseModel)

	var messages []Message
	if len(contents) == 0 {
		messages = []Message{{Role: "user", Content: Contents{{
			Type: "text",
			Text: "",
		}}}}
	} else {
		messages = []Message{{Role: "user", Content: contents}}
	}

	payload := map[string]any{
		"model":      p.baseModel,
		"max_tokens": 0,
		"system":     system,
		"stream":     false,
		"messages":   messages,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("warmup marshal", "err", err)
		return
	}

	resp, err := p.client.Post(fmt.Sprintf("%s%s", p.upstreamURL, messagesPath), jsonContentType, bytes.NewReader(jsonBody))
	if err != nil {
		slog.Warn("warmup request", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("warmup response", "status", resp.StatusCode)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	slog.Info("warmup done", "status", resp.StatusCode)
}

func (p *Proxy) eraseCache() {
	reqURL := fmt.Sprintf("%s%s?action=erase", p.upstreamURL, slotPath)

	slog.Info("erase cache", "url", reqURL)

	resp, err := p.client.Post(reqURL, jsonContentType, nil)
	if err != nil {
		slog.Warn("erase cache", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("erase cache", "status", resp.StatusCode)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("read erase response", "err", err)
		return
	}
	slog.Info("erase response", "status", resp.StatusCode, "body", string(respBody))
}

func (p *Proxy) restoreCache(filename string) {
	filename = escapeFilenameInline(filename)
	reqURL := fmt.Sprintf("%s%s?action=restore", p.upstreamURL, slotPath)

	slog.Info("restore cache", "url", reqURL, "filename", filename)

	reqBody := fmt.Appendf(nil, `{"filename":%q}`, filename)
	resp, err := p.client.Post(reqURL, jsonContentType, bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache restore", "filename", filename, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("cache restore", "status", resp.StatusCode)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("read restore response", "err", err)
		return
	}
	slog.Info("restore response", "status", resp.StatusCode, "body", string(respBody))
}

func (p *Proxy) saveCache(filename string) {
	filename = escapeFilenameInline(filename)
	for i := 0; i < maxSaveRetries; i++ {
		reqURL := fmt.Sprintf("%s%s?action=save", p.upstreamURL, slotPath)

		slog.Info("save cache", "url", reqURL, "filename", filename)

		reqBody := fmt.Appendf(nil, `{"filename":%q}`, filename)
		resp, err := p.client.Post(reqURL, jsonContentType, bytes.NewReader(reqBody))
		if err != nil {
			slog.Warn("cache save", "filename", filename, "err", err)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.Warn("cache save", "status", resp.StatusCode)
			resp.Body.Close()
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			slog.Warn("read save response", "err", err)
			continue
		}
		slog.Info("save response", "status", resp.StatusCode, "body", string(respBody))

		break
	}
}

// cleanupSystemCaches removes old system cache files for a given model
// when the number of caches exceeds the configured limit.
// The newly saved cache (newCacheName) is always preserved.
func (p *Proxy) cleanupSystemCaches(model, newCacheName string) {
	if p.systemCacheLimit <= 1 {
		return
	}

	limit := p.systemCacheLimit - 1

	model = escapeFilenameInline(model)
	newCacheName = escapeFilenameInline(newCacheName)

	entries, err := os.ReadDir(p.slotSavePath)
	if err != nil {
		slog.Warn("read cache dir", "err", err)
		return
	}

	type cacheFile struct {
		name    string
		modTime time.Time
	}

	var candidates []cacheFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Name() == newCacheName {
			continue
		}
		if !strings.HasPrefix(e.Name(), model+systemCacheSeparator) || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("failed to get file info", "err", err)
			continue
		}
		candidates = append(candidates, cacheFile{
			name:    e.Name(),
			modTime: info.ModTime(),
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.Before(candidates[j].modTime)
	})

	for len(candidates) > limit {
		old := candidates[0]
		candidates = candidates[1:]
		filePath := filepath.Join(p.slotSavePath, old.name)

		if err := os.Remove(filePath); err != nil {
			slog.Warn("failed to remove old cache", "file", old.name, "err", err)
			continue
		}

		ckptPath := filePath + ".ckpt"
		if err := os.Remove(ckptPath); err != nil {
			slog.Warn("failed to remove old cache ckpt", "file", ckptPath, "err", err)
		}

		slog.Info("removed old system cache", "file", old.name)
	}
}

// escapeFilenameInline replaces colons with double-dashes to avoid filesystem issues.
// Inlined because it's a trivial single-use transformation.
func escapeFilenameInline(name string) string {
	return strings.ReplaceAll(name, ":", "--")
}

// isUserOnlyConversation returns true if all messages in the conversation are from the user.
func isUserOnlyConversation(messages []Message) bool {
	for _, m := range messages {
		if m.Role != "user" {
			return false
		}
	}
	return true
}

// extractSystemInstructions extracts system instruction messages from a conversation.
// Returns all but the last content block from a single-message conversation.
func extractSystemInstructions(messages []Message) []Content {
	if len(messages) != 1 {
		return nil
	}

	var contents []Content
	for _, m := range messages {
		contents = append(contents, m.Content...)
	}

	if len(contents) <= 1 {
		return nil
	}

	return contents[0 : len(contents)-1]
}

func systemCacheFileExists(slotSavePath, filename string) bool {
	filename = escapeFilenameInline(filename)
	file := filepath.Join(slotSavePath, filename)
	slog.Info("test file", "path", file)

	info, err := os.Stat(file)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// hashSystem computes an FNV-32a hash of system prompt, tools, and instructions.
// Returns an error if instructions cannot be marshaled to JSON.
func hashSystem(system json.RawMessage, tools json.RawMessage, instructions []Content) (uint32, error) {
	instrs, err := json.Marshal(&instructions)
	if err != nil {
		return 0, fmt.Errorf("marshal instructions: %w", err)
	}

	h := fnv.New32a()
	h.Write(instrs)
	h.Write(system)
	h.Write(tools)
	return h.Sum32(), nil
}
