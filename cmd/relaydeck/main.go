// Command relaydeck is the RelayDeck webhook gateway & event relay CLI.
//
// Usage:
//
//	relaydeck serve [--config relaydeck.yaml] [--listen :8080] [--data-dir ./data]
//	relaydeck version
//	relaydeck config validate [--config relaydeck.yaml]
//	relaydeck events list [--base-url http://localhost:8080]
//	relaydeck events inspect <id>
//	relaydeck events replay <id>
//	relaydeck destinations list
//	relaydeck health [--base-url ...]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mrpythonist/relaydeck/internal/api"
	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/metrics"
	"github.com/mrpythonist/relaydeck/internal/security"
	"github.com/mrpythonist/relaydeck/internal/workers"
)

// Version is set at build time via -ldflags -X main.Version=...
var Version = "0.1.0-dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Printf("relaydeck %s\n", Version)
	case "config":
		err = runConfig(os.Args[2:])
	case "events":
		err = runEvents(os.Args[2:])
	case "destinations":
		err = runDestinations(os.Args[2:])
	case "health":
		err = runHealth(os.Args[2:])
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`relaydeck — Never lose a webhook. Never miss an event.

Usage:
  relaydeck serve [--config FILE] [--listen ADDR] [--data-dir DIR]
  relaydeck version
  relaydeck config validate [--config FILE]
  relaydeck events list [--base-url URL]
  relaydeck events inspect <id> [--base-url URL]
  relaydeck events replay <id> [--base-url URL]
  relaydeck destinations list [--base-url URL]
  relaydeck health [--base-url URL]

Examples:
  relaydeck serve
  relaydeck serve --config relaydeck.yaml --listen :8080
  relaydeck events list --base-url http://localhost:8080
  curl -X POST localhost:8080/api/v1/webhooks/stripe -d '{"type":"payment_intent.succeeded"}'

Environment: RELAYDECK_LISTEN_ADDR, RELAYDECK_DATA_DIR, RELAYDECK_API_KEY,
  RELAYDECK_WORKERS, RELAYDECK_MAX_PAYLOAD_BYTES, RELAYDECK_ALLOW_PRIVATE_DESTINATIONS
`)
}

// parseBaseURL extracts --base-url (either "--base-url URL" or
// "--base-url=URL") from args regardless of position, since Go's flag
// package stops parsing at the first positional argument.
func parseBaseURL(args []string) (string, []string) {
	base := "http://localhost:8080"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--base-url" && i+1 < len(args) {
			base = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--base-url=") {
			base = strings.TrimPrefix(a, "--base-url=")
			continue
		}
		rest = append(rest, a)
	}
	return base, rest
}

func apiKeyFromEnv() string { return os.Getenv("RELAYDECK_API_KEY") }

func httpGet(url string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if k := apiKeyFromEnv(); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

func httpPost(url string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if k := apiKeyFromEnv(); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

func printJSON(b []byte) {
	var v any
	if err := json.Unmarshal(b, &v); err == nil {
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Println(string(b))
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgFile := fs.String("config", os.Getenv("RELAYDECK_CONFIG"), "Path to YAML config file")
	listen := fs.String("listen", "", "Listen address (e.g. :8080)")
	dataDir := fs.String("data-dir", "", "Data directory for SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(cfg.DataDir, "relaydeck.db")
	store, err := database.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	// Seed sources + destinations from config.
	for _, src := range cfg.Sources {
		secret := os.ExpandEnv(src.Secret)
		if err := store.UpsertSource(src.Name, secret); err != nil {
			return fmt.Errorf("seed source %s: %w", src.Name, err)
		}
	}
	for _, d := range cfg.Destinations {
		if existing, _ := store.GetDestinationByName(d.Name); existing != nil {
			continue
		}
		enabled := true
		if d.Enabled != nil {
			enabled = *d.Enabled
		}
		method := d.Method
		if method == "" {
			method = "POST"
		}
		timeout := int(d.Timeout.Seconds())
		if timeout <= 0 {
			timeout = 10
		}
		maxRetries := d.MaxRetries
		if maxRetries <= 0 {
			maxRetries = 5
		}
		base := d.BackoffBase
		if base <= 0 {
			base = time.Second
		}
		max := d.BackoffMax
		if max <= 0 {
			max = time.Minute
		}
		if err := security.ValidateDestinationURL(d.URL, cfg.AllowPrivate, cfg.TrustedInternal, nil); err != nil {
			return fmt.Errorf("seed destination %q: %w", d.Name, err)
		}
		id, _ := security.NewID("dst_")
		dest := &database.Destination{
			ID: id, Name: d.Name, SourceFilter: d.Source, URL: d.URL, Method: strings.ToUpper(method),
			Headers: d.Headers, TimeoutSeconds: timeout, MaxRetries: maxRetries,
			BackoffBaseMs: base.Milliseconds(), BackoffMaxMs: max.Milliseconds(),
			Filter: d.Filter, Enabled: enabled,
		}
		if dest.Headers == nil {
			dest.Headers = map[string]string{}
		}
		if err := store.CreateDestination(dest); err != nil {
			return fmt.Errorf("seed destination %s: %w", d.Name, err)
		}
	}

	m := metrics.New()
	srv := api.New(store, cfg, m)
	pool := workers.New(store, delivery.NewSender(cfg.AllowPrivate, cfg.TrustedInternal), m, cfg.Workers, "worker-1")

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go pool.Run(ctx)
	go func() {
		slog.Info("relaydeck listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir, "workers", cfg.Workers, "version", Version)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	slog.Info("shutting down gracefully…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Workers drain via ctx cancellation inside pool.Run (semaphore drain).
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func runConfig(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return fmt.Errorf("usage: relaydeck config validate [--config FILE]")
	}
	fs := flag.NewFlagSet("config validate", flag.ExitOnError)
	cfgFile := fs.String("config", os.Getenv("RELAYDECK_CONFIG"), "Path to YAML config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	fmt.Printf("config valid: listen=%s data_dir=%s workers=%d max_payload_bytes=%d sources=%d destinations=%d\n",
		cfg.ListenAddr, cfg.DataDir, cfg.Workers, cfg.MaxPayloadBytes, len(cfg.Sources), len(cfg.Destinations))
	return nil
}

func runEvents(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: relaydeck events list|inspect <id>|replay <id>")
	}
	switch args[0] {
	case "list":
		base, _ := parseBaseURL(args[1:])
		b, code, err := httpGet(base + "/api/v1/events?limit=50")
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("server returned %d: %s", code, string(b))
		}
		printJSON(b)
		return nil
	case "inspect":
		base, rest := parseBaseURL(args[1:])
		if len(rest) < 1 {
			return fmt.Errorf("usage: relaydeck events inspect <id>")
		}
		b, code, err := httpGet(base + "/api/v1/events/" + rest[0])
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("server returned %d: %s", code, string(b))
		}
		printJSON(b)
		return nil
	case "replay":
		base, rest := parseBaseURL(args[1:])
		if len(rest) < 1 {
			return fmt.Errorf("usage: relaydeck events replay <id>")
		}
		b, code, err := httpPost(base + "/api/v1/events/" + rest[0] + "/replay")
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("server returned %d: %s", code, string(b))
		}
		printJSON(b)
		return nil
	default:
		return fmt.Errorf("unknown events subcommand %q", args[0])
	}
}

func runDestinations(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("usage: relaydeck destinations list [--base-url URL]")
	}
	base, _ := parseBaseURL(args[1:])
	b, code, err := httpGet(base + "/api/v1/destinations")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("server returned %d: %s", code, string(b))
	}
	printJSON(b)
	return nil
}

func runHealth(args []string) error {
	base, _ := parseBaseURL(args)
	b, code, err := httpGet(base + "/health")
	if err != nil {
		return err
	}
	fmt.Printf("health: HTTP %d %s\n", code, strings.TrimSpace(string(b)))
	if code != http.StatusOK {
		return fmt.Errorf("unhealthy")
	}
	return nil
}
