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
	"sync"
)

type Proxy struct {
	proxy          *httputil.ReverseProxy
	upstream       string
	model          string
	slotSavePath   string
	dumpDir        string
	client         http.Client
	mut            *sync.Mutex
	modelCacheName string
}

func New(upstream *url.URL, model, slotSavePath, dumpDir string) *Proxy {
	return &Proxy{
		proxy:        httputil.NewSingleHostReverseProxy(upstream),
		upstream:     upstream.String(),
		model:        model,
		slotSavePath: slotSavePath,
		dumpDir:      dumpDir,
		mut:          new(sync.Mutex),
	}
}

type requestBody struct {
	System    json.RawMessage   `json:"system"`
	Tools     json.RawMessage   `json:"tools"`
	Messages  []json.RawMessage `json:"messages"`
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Stream    bool              `json:"stream"`
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mut.Lock()
	defer p.mut.Unlock()

	slog.Info("proxy", "method", r.Method, "path", r.URL.Path, "upstream", p.upstream)

	if p.model != "" && r.URL.Path == "/v1/messages" {
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

		modelCache := p.model + "--chat.bin"
		systemHash := hashSystem(req.System, req.Tools)
		systemCache := fmt.Sprintf("%s--system--%x.bin", p.model, systemHash)
		switchedModel := p.modelCacheName != modelCache
		p.dumpRequest(systemHash, body)

		switch {
		case !systemCacheFileExists(p.slotSavePath, systemCache):
			p.eraseCache()
			p.warmupSystem(req.System, req.Tools)
			p.saveCache(systemCache)
		case switchedModel && len(req.Messages) > 1:
			// Continuation: restore model cache
			p.restoreCache(modelCache)
		case switchedModel:
			// New conversation: restore system cache
			p.restoreCache(systemCache)
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		p.proxy.ServeHTTP(w, r)

		p.saveCache(modelCache)
		p.modelCacheName = modelCache

		return
	}

	p.proxy.ServeHTTP(w, r)
}

func systemCacheFileExists(slotSavePath, filename string) bool {
	file := filepath.Join(slotSavePath, filename)
	slog.Info("test file", "path", file)

	info, err := os.Stat(file)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

func hashSystem(system json.RawMessage, tools json.RawMessage) uint32 {
	h := fnv.New32a()
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
	filename := fmt.Sprintf("%s----%x----%x.json", p.model, systemHash, reqHash)
	filepath := filepath.Join(p.dumpDir, filename)

	if err := os.MkdirAll(p.dumpDir, 0755); err != nil {
		slog.Warn("create dump dir", "err", err)
		return
	}

	if err := os.WriteFile(filepath, body, 0644); err != nil {
		slog.Warn("write dump", "filename", filename, "err", err)
	}
}

func (p *Proxy) warmupSystem(system json.RawMessage, tools json.RawMessage) {
	slog.Info("warmup system", "model", p.model)

	payload := map[string]any{
		"model":      p.model,
		"max_tokens": 0,
		"system":     system,
		"stream":     false,
		"messages":   []map[string]string{},
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
	reqURL := fmt.Sprintf("%s/slots/0?action=save", p.upstream)

	slog.Info("save cache", "url", reqURL, "filename", filename)

	reqBody := fmt.Appendf(nil, `{"filename":%q}`, filename)
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache save", "filename", filename, "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("save response", "status", resp.StatusCode, "body", string(respBody))
}
