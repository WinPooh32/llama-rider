package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/WinPooh32/llama-rider/proxy"
)

func main() {
	port := flag.String("port", "8080", "proxy listen port")
	upstream := flag.String("upstream", "http://localhost:9091", "llama.cpp upstream URL")
	flag.Parse()

	u, err := proxy.ParseUpstream(*upstream)
	if err != nil {
		slog.Error("invalid upstream URL", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy.New(u))

	addr := fmt.Sprintf(":%s", *port)
	slog.Info("proxy listening", "addr", addr, "upstream", u)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
