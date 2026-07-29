package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/WinPooh32/llama-rider/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	port := flag.String("port", "8080", "proxy listen port")
	systemCacheLimit := flag.Int("system-cache-limit", 0, "max system caches per model (0 = unlimited)")
	flag.Parse()

	llamaArgs := flag.Args()
	if len(llamaArgs) == 0 {
		slog.Error("usage: llama-rider -port <port> /path/to/llama-server [args...]")
		os.Exit(1)
	}

	var err error
	llamaPort, err := extractArg(llamaArgs, "port")
	if err != nil {
		slog.Error("extract arg", "err", err)
		os.Exit(1)
	}

	llamaAlias, err := extractArg(llamaArgs, "alias")
	if err != nil {
		slog.Error("extract arg", "err", err)
		os.Exit(1)
	}

	slotSavePath, err := extractArg(llamaArgs, "slot-save-path")
	if err != nil {
		slog.Error("extract arg", "err", err)
		os.Exit(1)
	}

	dumpDir := "dumps"
	upstream, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%s", llamaPort))
	if err != nil {
		slog.Error("parse upstream URL", "err", err)
		os.Exit(1)
	}

	cmd := exec.Command(llamaArgs[0], llamaArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Prevent llama-server catch signals from root process
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	if err := cmd.Start(); err != nil {
		slog.Error("start llama-server", "err", err)
		os.Exit(1)
	}

	slog.Info("proxy started", "upstream", upstream, "alias", llamaAlias, "slotSavePath", slotSavePath, "dumpDir", dumpDir)

	mux := http.NewServeMux()
	prx := proxy.New(upstream, llamaAlias, slotSavePath, dumpDir, *systemCacheLimit)
	mux.Handle("/", prx)

	addr := fmt.Sprintf(":%s", *port)
	slog.Info("proxy listening", "addr", addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	done := make(chan struct{})

	go func() {
		defer stop()

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
		}

		slog.Info("send exit signal to llama-server")
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			slog.Error("signal llama-server to quit", "err", err)
		}
	}()

	go func() {
		<-ctx.Done()
		slog.Info("got exit signal")

		prx.SaveChatCache()

		slog.Info("stopping http server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http shutdown is timed out", "err", err)
		}
	}()

	go func() {
		defer close(done)
		defer stop()

		if err := cmd.Wait(); err != nil {
			slog.Error("wait llama-server", "err", err)
		}
	}()

	<-done
}

func extractArg(args []string, name string) (string, error) {
	prefix := "--" + name + "="
	for i, arg := range args {
		if arg == "--"+name && i+1 < len(args) {
			return args[i+1], nil
		}
		if val, ok := strings.CutPrefix(arg, prefix); ok {
			return val, nil
		}
	}
	return "", fmt.Errorf("could not find --%s in llama-server arguments", name)
}
