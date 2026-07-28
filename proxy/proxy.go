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
	"strings"
	"sync"
	"time"
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

type Proxy struct {
	proxy          *httputil.ReverseProxy
	upstream       string
	baseModel      string
	slotSavePath   string
	dumpDir        string
	client         *http.Client
	mut            *sync.Mutex
	modelCacheName string
	closeCh        <-chan struct{}
}

func New(upstream *url.URL, model, slotSavePath, dumpDir string, closeCh <-chan struct{}) *Proxy {
	return &Proxy{
		proxy:        httputil.NewSingleHostReverseProxy(upstream),
		upstream:     upstream.String(),
		baseModel:    model,
		slotSavePath: slotSavePath,
		dumpDir:      dumpDir,
		mut:          new(sync.Mutex),
		client: &http.Client{
			Timeout: 120 * time.Second,
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

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mut.Lock()
	defer p.mut.Unlock()

	slog.Info("proxy", "method", r.Method, "path", r.URL.Path, "upstream", p.upstream)

	if p.baseModel != "" && r.URL.Path == "/v1/messages" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("read body", "err", err)
			p.proxy.ServeHTTP(w, r)
			return
		}

		var req requestBody
		if err := json.Unmarshal(body, &req); err != nil {
			slog.Error("parse body", "err", err)
			r.Body = io.NopCloser(bytes.NewReader(body))
			p.proxy.ServeHTTP(w, r)
			return
		}

		instructions := instructMessages(req.Messages)

		modelCache := req.Model + "--chat.bin"
		systemHash := hashSystem(req.System, req.Tools, instructions)
		systemCache := fmt.Sprintf("%s--system--%x.bin", req.Model, systemHash)
		switchedModel := p.modelCacheName != modelCache
		p.dumpRequest(systemHash, body)

		switch {
		case !systemCacheFileExists(p.slotSavePath, systemCache):
			if p.modelCacheName != "" {
				// Save current model state before switch.
				p.saveCache(p.modelCacheName)
			}

			p.eraseCache()
			p.warmupSystem(req.System, req.Tools, instructions)
			p.saveCache(systemCache)
		case switchedModel && !onlyUser(req.Messages):
			if p.modelCacheName != "" {
				// Save current model state before switch.
				p.saveCache(p.modelCacheName)
			}

			// Continuation: restore model cache
			p.restoreCache(modelCache)
		case switchedModel:
			if p.modelCacheName != "" {
				// Save current model state before switch.
				p.saveCache(p.modelCacheName)
			}

			// New conversation: restore system cache
			p.restoreCache(systemCache)
		default:
			// Not model switch and not new chat
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		p.proxy.ServeHTTP(w, r)

		select {
		case <-p.closeCh:
			// Save chat cache only on close signal
			p.saveCache(modelCache)
		default:
		}

		p.modelCacheName = modelCache

		return
	}

	p.proxy.ServeHTTP(w, r)
}

func (p *Proxy) SaveChatCache() {
	if !p.mut.TryLock() {
		// Request should save chat cache on exit.
		return
	}
	defer p.mut.Unlock()

	if p.modelCacheName == "" {
		return
	}

	p.saveCache(p.modelCacheName)
}

func onlyUser(messages []Message) bool {
	for _, m := range messages {
		if m.Role != "user" {
			return false
		}
	}

	return true
}

func instructMessages(messages []Message) []Content {
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
	filename = escapeFilename(filename)
	file := filepath.Join(slotSavePath, filename)
	slog.Info("test file", "path", file)

	info, err := os.Stat(file)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

func hashSystem(system json.RawMessage, tools json.RawMessage, instructions []Content) uint32 {
	instrs, _ := json.Marshal(&instructions)

	h := fnv.New32a()
	h.Write(instrs)
	h.Write(system)
	h.Write(tools)
	return h.Sum32()
}

func hashRequest(body []byte) uint32 {
	h := fnv.New32a()
	h.Write(body)
	return h.Sum32()
}

func (p *Proxy) dumpRequest(systemHash uint32, body []byte) {
	reqHash := hashRequest(body)
	filename := fmt.Sprintf("%s----%x----%x.json", p.baseModel, systemHash, reqHash)
	filename = escapeFilename(filename)
	filepath := filepath.Join(p.dumpDir, filename)

	if err := os.MkdirAll(p.dumpDir, 0755); err != nil {
		slog.Warn("create dump dir", "err", err)
		return
	}

	if err := os.WriteFile(filepath, body, 0644); err != nil {
		slog.Warn("write dump", "filename", filename, "err", err)
	}
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

	resp, err := p.client.Post(fmt.Sprintf("%s/v1/messages", p.upstream), "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		slog.Warn("warmup request", "err", err)
		return
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)
	slog.Info("warmup done", "status", resp.StatusCode)
}

func (p *Proxy) eraseCache() {
	reqURL := fmt.Sprintf("%s/slots/0?action=erase", p.upstream)

	slog.Info("erase cache", "url", reqURL)

	resp, err := p.client.Post(reqURL, "application/json", nil)
	if err != nil {
		slog.Warn("erase restore", "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("erase response", "status", resp.StatusCode, "body", string(respBody))
}

func (p *Proxy) restoreCache(filename string) {
	filename = escapeFilename(filename)
	reqURL := fmt.Sprintf("%s/slots/0?action=restore", p.upstream)

	slog.Info("restore cache", "url", reqURL, "filename", filename)

	reqBody := fmt.Appendf(nil, `{"filename":%q}`, filename)
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache restore", "filename", filename, "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("restore response", "status", resp.StatusCode, "body", string(respBody))
}

func (p *Proxy) saveCache(filename string) {
	filename = escapeFilename(filename)
	for range 3 {
		reqURL := fmt.Sprintf("%s/slots/0?action=save", p.upstream)

		slog.Info("save cache", "url", reqURL, "filename", filename)

		reqBody := fmt.Appendf(nil, `{"filename":%q}`, filename)
		resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
		if err != nil {
			slog.Warn("cache save", "filename", filename, "err", err)
			continue
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		slog.Info("save response", "status", resp.StatusCode, "body", string(respBody))

		break
	}
}

func escapeFilename(name string) string {
	return strings.ReplaceAll(name, ":", "--")
}
