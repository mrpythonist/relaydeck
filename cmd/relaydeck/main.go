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
	"bytes"
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
	"github.com/mrpythonist/relaydeck/internal/plans"
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
	case "prune":
		err = runPrune(os.Args[2:])
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
  relaydeck prune [--config FILE] [--data-dir DIR] [--workspace ID]
  relaydeck version
  relaydeck config validate [--config FILE]
  relaydeck events list [--base-url URL]
  relaydeck events inspect <id> [--base-url URL]
  relaydeck events replay <id> [--base-url URL]
  relaydeck destinations list [--base-url URL]
  relaydeck destinations create --name NAME --url URL [flags]
  relaydeck destinations update <id-or-name> [flags]
  relaydeck destinations delete <id-or-name>
  relaydeck health [--base-url URL]

Examples:
  relaydeck serve
  relaydeck serve --config relaydeck.yaml --listen :8080
  relaydeck prune --workspace ws_abc123
  relaydeck events list --base-url http://localhost:8080
  curl -X POST localhost:8080/api/v1/webhooks/stripe -d '{"type":"payment_intent.succeeded"}'

Environment: RELAYDECK_LISTEN_ADDR, RELAYDECK_DATA_DIR, RELAYDECK_API_KEY,
  RELAYDECK_WORKERS, RELAYDECK_MAX_PAYLOAD_BYTES, RELAYDECK_ALLOW_PRIVATE_DESTINATIONS,
  RELAYDECK_HOSTED=true, STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET,
  STRIPE_PRICE_STARTER, STRIPE_PRICE_GROWTH
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
			Filter: d.Filter, JSONPathFilter: d.JSONFilter, SigningSecret: os.ExpandEnv(d.SigningSecret),
			HeaderTemplates: d.HeaderTemplates, Enabled: enabled,
		}
		if dest.Headers == nil {
			dest.Headers = map[string]string{}
		}
		if dest.HeaderTemplates == nil {
			dest.HeaderTemplates = map[string]string{}
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
	// Retention pruning: every hour, delete events older than each
	// workspace's plan retention (Free 3 days, Starter 30, Growth 90).
	// Self-hosted mode uses the configured RELAYDECK_RETENTION_DAYS.
	go runPruner(ctx, store, cfg)
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

func planRetentionMap() map[string]int {
	return map[string]int{
		"free":    plans.Free.RetentionDays,
		"starter": plans.Starter.RetentionDays,
		"growth":  plans.Growth.RetentionDays,
	}
}

func runPruner(ctx context.Context, store *database.Store, cfg *config.Config) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := store.PruneAllWorkspaces(planRetentionMap(), cfg.RetentionDays)
			if err != nil {
				slog.Error("retention prune failed", "error", err)
			} else if n > 0 {
				slog.Info("retention prune done", "events_deleted", n)
			}
		}
	}
}

func runPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	cfgFile := fs.String("config", os.Getenv("RELAYDECK_CONFIG"), "Path to YAML config file")
	dataDir := fs.String("data-dir", "", "Data directory for SQLite database")
	workspaceID := fs.String("workspace", "", "Prune only this workspace id (default: all workspaces)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := database.Open(filepath.Join(cfg.DataDir, "relaydeck.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	if *workspaceID != "" {
		ws, err := store.GetWorkspace(*workspaceID)
		if err != nil || ws == nil {
			return fmt.Errorf("workspace %q not found", *workspaceID)
		}
		n, err := store.DeleteOldEventsForWorkspace(ws.ID, plans.Get(ws.Plan).RetentionDays)
		if err != nil {
			return err
		}
		fmt.Printf("pruned workspace %s (plan %s, retention %d days): %d events deleted\n", ws.ID, ws.Plan, plans.Get(ws.Plan).RetentionDays, n)
		return nil
	}
	n, err := store.PruneAllWorkspaces(planRetentionMap(), cfg.RetentionDays)
	if err != nil {
		return err
	}
	fmt.Printf("pruned all workspaces: %d events deleted\n", n)
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
	if len(args) == 0 {
		return fmt.Errorf("usage: relaydeck destinations list|create|update <id-or-name>|delete <id-or-name>")
	}
	switch args[0] {
	case "list":
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
	case "create":
		return runDestinationsCreate(args[1:])
	case "update":
		return runDestinationsUpdate(args[1:])
	case "delete":
		return runDestinationsDelete(args[1:])
	default:
		return fmt.Errorf("unknown destinations subcommand %q", args[0])
	}
}

// kvFlag is a repeatable --flag name=value option.
type kvFlag map[string]string

func (f kvFlag) String() string { return fmt.Sprint(map[string]string(f)) }
func (f kvFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("expected name=value, got %q", v)
	}
	f[strings.TrimSpace(k)] = val
	return nil
}

// httpReq sends a request with an optional JSON body and the API key.
func httpReq(method, url string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

// resolveDestinationID maps a destination id or name to its id.
func resolveDestinationID(base, nameOrID string) (string, string, error) {
	b, code, err := httpGet(base + "/api/v1/destinations")
	if err != nil {
		return "", "", err
	}
	if code != http.StatusOK {
		return "", "", fmt.Errorf("server returned %d: %s", code, string(b))
	}
	var out struct {
		Destinations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"destinations"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", fmt.Errorf("parse destinations: %w", err)
	}
	for _, d := range out.Destinations {
		if d.ID == nameOrID || d.Name == nameOrID {
			return d.ID, d.Name, nil
		}
	}
	return "", "", fmt.Errorf("destination %q not found", nameOrID)
}

func runDestinationsCreate(args []string) error {
	fs := flag.NewFlagSet("destinations create", flag.ExitOnError)
	name := fs.String("name", "", "Destination name (required)")
	url := fs.String("url", "", "Destination URL (required)")
	source := fs.String("source", "", "Source filter (empty = all sources)")
	method := fs.String("method", "POST", "HTTP method")
	filter := fs.String("filter", "", "Payload substring filter")
	jsonFilter := fs.String("json-filter", "", "JSON-path filter, e.g. type=payment_intent.succeeded")
	signingSecret := fs.String("signing-secret", "", "Outbound HMAC signing secret (or ${VAR} from env)")
	timeout := fs.Int("timeout", 10, "Request timeout in seconds")
	maxRetries := fs.Int("max-retries", 5, "Max delivery attempts")
	enabled := fs.Bool("enabled", true, "Enable the destination")
	headers := kvFlag{}
	templates := kvFlag{}
	fs.Var(headers, "header", "Static header name=value (repeatable)")
	fs.Var(templates, "header-template", "Header template name=template (repeatable, {{path}} from payload)")
	base, rest := parseBaseURL(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*url) == "" {
		return fmt.Errorf("--name and --url are required")
	}
	body, _ := json.Marshal(map[string]any{
		"name": *name, "url": *url, "source_filter": *source, "method": strings.ToUpper(*method),
		"filter_substring": *filter, "filter_jsonpath": *jsonFilter,
		"signing_secret": os.ExpandEnv(*signingSecret), "header_templates": templates,
		"headers": headers, "timeout_seconds": *timeout, "max_retries": *maxRetries,
		"enabled": *enabled,
	})
	b, code, err := httpReq(http.MethodPost, base+"/api/v1/destinations", body)
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return fmt.Errorf("server returned %d: %s", code, string(b))
	}
	printJSON(b)
	return nil
}

func runDestinationsUpdate(args []string) error {
	fs := flag.NewFlagSet("destinations update", flag.ExitOnError)
	url := fs.String("url", "", "New destination URL")
	source := fs.String("source", "", "New source filter")
	method := fs.String("method", "", "New HTTP method")
	filter := fs.String("filter", "", "New payload substring filter")
	jsonFilter := fs.String("json-filter", "", "New JSON-path filter")
	signingSecret := fs.String("signing-secret", "", "New outbound HMAC signing secret (or ${VAR} from env)")
	timeout := fs.Int("timeout", 0, "New request timeout in seconds")
	maxRetries := fs.Int("max-retries", 0, "New max delivery attempts")
	enabled := fs.Bool("enabled", true, "Enable/disable the destination")
	clearFilter := fs.Bool("clear-filter", false, "Clear the substring filter")
	clearJSONFilter := fs.Bool("clear-json-filter", false, "Clear the JSON-path filter")
	clearSigningSecret := fs.Bool("clear-signing-secret", false, "Clear the signing secret")
	clearTemplates := fs.Bool("clear-header-templates", false, "Clear all header templates")
	headers := kvFlag{}
	templates := kvFlag{}
	fs.Var(headers, "header", "Set/merge static header name=value (repeatable)")
	fs.Var(templates, "header-template", "Set/merge header template name=template (repeatable)")
	base, rest := parseBaseURL(args)
	if len(rest) < 1 {
		return fmt.Errorf("usage: relaydeck destinations update <id-or-name> [flags]")
	}
	// The positional comes first: Go's flag package stops parsing at the
	// first non-flag argument, so flags must be parsed from rest[1:].
	nameOrID := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	id, name, err := resolveDestinationID(base, nameOrID)
	if err != nil {
		return err
	}
	// Fetch the current object, apply only the flags that were set, and
	// PUT the whole object back. This keeps fields the caller did not
	// touch (including enabled) intact.
	b, code, err := httpGet(base + "/api/v1/destinations/" + id)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("server returned %d: %s", code, string(b))
	}
	var cur map[string]any
	if err := json.Unmarshal(b, &cur); err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["url"] {
		cur["url"] = *url
	}
	if set["source"] {
		cur["source_filter"] = *source
	}
	if set["method"] {
		cur["method"] = strings.ToUpper(*method)
	}
	if set["filter"] {
		cur["filter_substring"] = *filter
	}
	if set["json-filter"] {
		cur["filter_jsonpath"] = *jsonFilter
	}
	if set["signing-secret"] {
		cur["signing_secret"] = os.ExpandEnv(*signingSecret)
	}
	if set["timeout"] {
		cur["timeout_seconds"] = *timeout
	}
	if set["max-retries"] {
		cur["max_retries"] = *maxRetries
	}
	if set["enabled"] {
		cur["enabled"] = *enabled
	}
	for k, v := range headers {
		h, _ := cur["headers"].(map[string]any)
		if h == nil {
			h = map[string]any{}
		}
		h[k] = v
		cur["headers"] = h
	}
	for k, v := range templates {
		t, _ := cur["header_templates"].(map[string]any)
		if t == nil {
			t = map[string]any{}
		}
		t[k] = v
		cur["header_templates"] = t
	}
	// The fetched signing secret is "***REDACTED***"; the server leaves it
	// unchanged when it sees that value.
	body, _ := json.Marshal(cur)
	path := base + "/api/v1/destinations/" + id
	var q []string
	if *clearFilter {
		q = append(q, "clear_filter=1")
	}
	if *clearJSONFilter {
		q = append(q, "clear_jsonpath=1")
	}
	if *clearSigningSecret {
		q = append(q, "clear_signing_secret=1")
	}
	if *clearTemplates {
		q = append(q, "clear_header_templates=1")
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	b, code, err = httpReq(http.MethodPut, path, body)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("server returned %d: %s", code, string(b))
	}
	printJSON(b)
	fmt.Printf("updated destination %s (%s)\n", name, id)
	return nil
}

func runDestinationsDelete(args []string) error {
	base, rest := parseBaseURL(args)
	if len(rest) < 1 {
		return fmt.Errorf("usage: relaydeck destinations delete <id-or-name>")
	}
	id, name, err := resolveDestinationID(base, rest[0])
	if err != nil {
		return err
	}
	b, code, err := httpReq(http.MethodDelete, base+"/api/v1/destinations/"+id, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("server returned %d: %s", code, string(b))
	}
	fmt.Printf("deleted destination %s (%s)\n", name, id)
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
