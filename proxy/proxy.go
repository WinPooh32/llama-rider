package proxy

import (
	"bytes"
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

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("proxy", "method", r.Method, "path", r.URL.Path, "upstream", p.upstream)

	if p.model != "" && r.URL.Path == "/v1/messages" {
		p.restoreCache(p.model)
		defer p.saveCache(p.model)
	}

	p.proxy.ServeHTTP(w, r)
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
