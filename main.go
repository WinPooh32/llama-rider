package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"

	"github.com/WinPooh32/llama-rider/proxy"
)

func main() {
	port := flag.String("port", "8080", "proxy listen port")
	flag.Parse()

	llamaArgs := flag.Args()
	if len(llamaArgs) == 0 {
		slog.Error("usage: llama-rider -port <port> /path/to/llama-server [args...]")
		os.Exit(1)
	}

	llamaPort := extractPort(llamaArgs)
	upstream, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d/", llamaPort))

	cmd := exec.Command(llamaArgs[0], llamaArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("start llama-server", "err", err)
		os.Exit(1)
	}
	defer cmd.Wait()

	slog.Info("proxy started", "upstream", upstream)

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy.New(upstream))

	addr := fmt.Sprintf(":%s", *port)
	slog.Info("proxy listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

func extractPort(args []string) int {
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			var port int
			fmt.Sscanf(args[i+1], "%d", &port)
			return port
		}
	}
	slog.Error("could not find -port in llama-server arguments")
	os.Exit(1)
	return 0
}
