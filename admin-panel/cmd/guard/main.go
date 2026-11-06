// Command guard runs the local Luckfox security console.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"guard.local/admin/internal/app/console"
	delivery "guard.local/admin/internal/app/delivery/http"
	"guard.local/admin/internal/app/relay"
	"guard.local/admin/internal/app/repository"
	"guard.local/admin/internal/app/usecase"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	dataDir := flag.String("data", "data", "directory for settings and JSONL journal")
	flag.Parse()
	password, agentToken := os.Getenv("GUARD_ADMIN_PASSWORD"), os.Getenv("GUARD_AGENT_TOKEN")
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && host != "localhost" && password == "" {
		return errors.New("GUARD_ADMIN_PASSWORD is required when listening outside loopback")
	}
	if password != "" && len(password) < 12 {
		return errors.New("GUARD_ADMIN_PASSWORD must contain at least 12 bytes")
	}
	if agentToken != "" && len(agentToken) < 24 {
		return errors.New("GUARD_AGENT_TOKEN must contain at least 24 bytes")
	}
	if err = os.MkdirAll(*dataDir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	journal, err := repository.NewJournal(*dataDir)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// The existing go2rtc process owns streaming. This process only reads its
	// counters; the browser embeds its unmodified stream.html directly.
	go2rtcURL := "http://127.0.0.1:1984"
	engine := relay.New(go2rtcURL)
	defer engine.Close()
	service, err := usecase.New(repository.NewSettingsFile(*dataDir), journal, engine, agentToken != "", log)
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	var externalKnownHosts []string
	if userDir, err := os.UserHomeDir(); err == nil {
		externalKnownHosts = append(externalKnownHosts, filepath.Join(userDir, ".ssh", "known_hosts"))
	}
	sshConnector := &console.Connector{Keys: console.NewHostKeys(filepath.Join(*dataDir, "ssh_known_hosts"), externalKnownHosts...)}
	handler := delivery.New(service, log).WithConsole(sshConnector)
	server := &http.Server{Addr: *listen, Handler: handler.Routes(nil, password, agentToken), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	go service.Monitor(ctx)
	serveError := make(chan error, 1)
	go func() { serveError <- server.Serve(listener) }()
	absDir, _ := filepath.Abs(*dataDir)
	log.Info("guard console ready", "url", "http://"+*listen, "data", absDir, "agent_api", agentToken != "")
	select {
	case err = <-serveError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Cancelling BaseContext ends SSE and the monitor before shutdown.
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}
	return nil
}
