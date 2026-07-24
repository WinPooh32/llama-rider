package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Proxy struct {
	proxy    *httputil.ReverseProxy
	upstream string
	client   http.Client
}

func New(upstream *url.URL) *Proxy {
	return &Proxy{
		proxy:    httputil.NewSingleHostReverseProxy(upstream),
		upstream: upstream.String(),
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("proxy", "method", r.Method, "path", r.URL.Path, "upstream", p.upstream)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("read body", "err", err)
		p.proxy.ServeHTTP(w, r)
		return
	}

	model := extractModel(body)
	slog.Info("model", "name", model)
	if model != "" && r.URL.Path == "/v1/messages" {
		p.restoreCache(model)
		defer p.saveCache(model)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	p.proxy.ServeHTTP(w, r)
}

func extractModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Model
}

func (p *Proxy) restoreCache(model string) {
	filename := model + ".bin"
	reqURL := fmt.Sprintf("%s/slots/0?action=restore", p.upstream)

	slog.Info("restore cache", "url", reqURL, "filename", filename)

	reqBody := []byte(fmt.Sprintf(`{"filename":%q}`, filename))
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache restore", "model", model, "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("restore response", "status", resp.StatusCode, "body", string(respBody))
}

func (p *Proxy) saveCache(model string) {
	filename := model + ".bin"
	reqURL := fmt.Sprintf("%s/slots/0?action=save", p.upstream)

	slog.Info("save cache", "url", reqURL, "filename", filename)

	reqBody := []byte(fmt.Sprintf(`{"filename":%q}`, filename))
	resp, err := p.client.Post(reqURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("cache save", "model", model, "err", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	slog.Info("save response", "status", resp.StatusCode, "body", string(respBody))
}
