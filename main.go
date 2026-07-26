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
	"syscall"
	"time"

	"github.com/WinPooh32/llama-rider/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	port := flag.String("port", "8080", "proxy listen port")
	flag.Parse()

	llamaArgs := flag.Args()
	if len(llamaArgs) == 0 {
		slog.Error("usage: llama-rider -port <port> /path/to/llama-server [args...]")
		os.Exit(1)
	}

	llamaPort := extractArg(llamaArgs, "port")
	llamaAlias := extractArg(llamaArgs, "alias")
	slotSavePath := extractArg(llamaArgs, "slot-save-path")
	dumpDir := "dumps"
	upstream, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%s", llamaPort))

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
	mux.Handle("/", proxy.New(upstream, llamaAlias, slotSavePath, dumpDir))

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

func extractArg(args []string, name string) string {
	for i, arg := range args {
		if arg == fmt.Sprintf("--%s", name) && i+1 < len(args) {
			return args[i+1]
		}
	}
	slog.Error(fmt.Sprintf("could not find --%s in llama-server arguments", name))
	os.Exit(1)
	return ""
}
