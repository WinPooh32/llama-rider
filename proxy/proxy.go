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
)

type Proxy struct {
	proxy    *httputil.ReverseProxy
	upstream string
	model    string
	client   http.Client
}

func New(upstream *url.URL, model string) *Proxy {
	return &Proxy{
		proxy:    httputil.NewSingleHostReverseProxy(upstream),
		upstream: upstream.String(),
		model:    model,
	}
}

type requestBody struct {
	System   string         `json:"system"`
	Tools    json.RawMessage `json:"tools"`
	Model    string         `json:"model"`
	MaxToken int            `json:"max_tokens"`
	Stream   bool           `json:"stream"`
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

		cacheFile := p.model + ".bin"
		p.restoreCache(cacheFile)

		systemHash := hashSystem(req.System, req.Tools)
		warmupFile := fmt.Sprintf("%s----%x.bin", p.model, systemHash)

		// Try to restore system-specific cache
		restoreResp := p.tryRestoreCache(warmupFile)

		if !restoreResp {
			// Warm up the cache with the system prompt
			p.warmupSystem(req.System, req.Tools)
			p.saveCache(warmupFile)
		}

		defer p.saveCache(cacheFile)

		r.Body = io.NopCloser(bytes.NewReader(body))
		p.proxy.ServeHTTP(w, r)
		return
	}

	p.proxy.ServeHTTP(w, r)
}

func hashSystem(system string, tools json.RawMessage) uint32 {
	h := fnv.New32a()
	h.Write([]byte(system))
	h.Write(tools)
	return h.Sum32()
}

func (p *Proxy) warmupSystem(system string, tools json.RawMessage) {
	slog.Info("warmup system", "model", p.model)

	payload := map[string]interface{}{
		"model":      p.model,
		"max_tokens": 0,
		"system":     system,
		"stream":     false,
		"messages": []map[string]string{
			{"role": "user", "content": ""},
		},
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

func (p *Proxy) tryRestoreCache(filename string) bool {
	reqURL := fmt.Sprintf("%s/slots/0?action=restore", p.upstream)

	slog.Info("try restore cache", "url", reqURL, "filename", filename)

	reqBody := []byte(fmt.Sprintf(`{"filename":%q}`, filename))
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache restore", "filename", filename, "err", err)
		return false
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("restore response", "status", resp.StatusCode, "body", string(respBody))

	return resp.StatusCode == http.StatusOK
}

func (p *Proxy) restoreCache(filename string) {
	reqURL := fmt.Sprintf("%s/slots/0?action=restore", p.upstream)

	slog.Info("restore cache", "url", reqURL, "filename", filename)

	reqBody := []byte(fmt.Sprintf(`{"filename":%q}`, filename))
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

	reqBody := []byte(fmt.Sprintf(`{"filename":%q}`, filename))
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache save", "filename", filename, "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("save response", "status", resp.StatusCode, "body", string(respBody))
}
