// Package config loads RelayDeck configuration from YAML, environment
// variables (RELAYDECK_*) and CLI flags. Precedence: flags > env > YAML > defaults.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DestinationConfig is a seed destination from YAML config.
type DestinationConfig struct {
	Name        string            `yaml:"name"`
	Source      string            `yaml:"source"`
	URL         string            `yaml:"url"`
	Method      string            `yaml:"method"`
	Headers     map[string]string `yaml:"headers"`
	Timeout     time.Duration     `yaml:"timeout"`
	MaxRetries  int               `yaml:"max_retries"`
	BackoffBase time.Duration     `yaml:"backoff_base"`
	BackoffMax  time.Duration     `yaml:"backoff_max"`
	Enabled     *bool             `yaml:"enabled"`
	Filter      string            `yaml:"filter"`
}

// SourceConfig is a seed source (with optional HMAC secret) from YAML.
type SourceConfig struct {
	Name   string `yaml:"name"`
	Secret string `yaml:"secret"`
}

// File is the YAML configuration file format.
type File struct {
	ListenAddr      string              `yaml:"listen_addr"`
	DataDir         string              `yaml:"data_dir"`
	APIKey          string              `yaml:"api_key"`
	MaxPayloadBytes int64               `yaml:"max_payload_bytes"`
	Workers         int                 `yaml:"workers"`
	AllowPrivate    bool                `yaml:"allow_private_destinations"`
	TrustedInternal []string            `yaml:"trusted_internal_destinations"`
	RetentionDays   int                 `yaml:"retention_days"`
	Hosted          bool                `yaml:"hosted"`
	Sources         []SourceConfig      `yaml:"sources"`
	Destinations    []DestinationConfig `yaml:"destinations"`
}

// Config is the effective runtime configuration.
type Config struct {
	ListenAddr      string
	DataDir         string
	APIKey          string
	MaxPayloadBytes int64
	Workers         int
	AllowPrivate    bool
	TrustedInternal []string
	RetentionDays   int
	// Hosted enables multi-tenant mode. Default false keeps the old
	// single-tenant behaviour for self-hosters, with no breaking change.
	Hosted bool
	// Stripe billing (hosted only). Empty means billing is not configured.
	StripeSecretKey     string
	StripeWebhookSecret string
	StripePriceStarter  string
	StripePriceGrowth   string
	StripeSuccessURL    string
	StripeCancelURL     string
	Sources             []SourceConfig
	Destinations        []DestinationConfig
	ConfigFile          string
}

func Defaults() *Config {
	return &Config{
		ListenAddr:      ":8080",
		DataDir:         "./data",
		MaxPayloadBytes: 1 << 20, // 1 MiB
		Workers:         4,
		RetentionDays:   30,
	}
}

func expandEnv(s string) string { return os.ExpandEnv(s) }

// LoadFile reads a YAML config file if path is non-empty.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// Load builds the effective config: defaults, then YAML, then env, then flags.
// flags are applied by the caller via ApplyFlags.
func Load(configFile string) (*Config, error) {
	cfg := Defaults()
	cfg.ConfigFile = configFile
	if configFile != "" {
		f, err := LoadFile(configFile)
		if err != nil {
			return nil, err
		}
		applyFile(cfg, f)
	}
	applyEnv(cfg)
	return cfg, nil
}

func applyFile(cfg *Config, f *File) {
	if f.ListenAddr != "" {
		cfg.ListenAddr = f.ListenAddr
	}
	if f.DataDir != "" {
		cfg.DataDir = f.DataDir
	}
	if f.APIKey != "" {
		cfg.APIKey = expandEnv(f.APIKey)
	}
	if f.MaxPayloadBytes > 0 {
		cfg.MaxPayloadBytes = f.MaxPayloadBytes
	}
	if f.Workers > 0 {
		cfg.Workers = f.Workers
	}
	if f.AllowPrivate {
		cfg.AllowPrivate = true
	}
	if len(f.TrustedInternal) > 0 {
		cfg.TrustedInternal = f.TrustedInternal
	}
	if f.RetentionDays > 0 {
		cfg.RetentionDays = f.RetentionDays
	}
	if f.Hosted {
		cfg.Hosted = true
	}
	if len(f.Sources) > 0 {
		cfg.Sources = f.Sources
	}
	if len(f.Destinations) > 0 {
		cfg.Destinations = f.Destinations
	}
}

func getenv(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	return v, ok && strings.TrimSpace(v) != ""
}

func applyEnv(cfg *Config) {
	if v, ok := getenv("RELAYDECK_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := getenv("RELAYDECK_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	if v, ok := getenv("RELAYDECK_API_KEY"); ok {
		cfg.APIKey = v
	}
	if v, ok := getenv("RELAYDECK_MAX_PAYLOAD_BYTES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxPayloadBytes = n
		}
	}
	if v, ok := getenv("RELAYDECK_WORKERS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Workers = n
		}
	}
	if v, ok := getenv("RELAYDECK_ALLOW_PRIVATE_DESTINATIONS"); ok {
		cfg.AllowPrivate = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := getenv("RELAYDECK_TRUSTED_INTERNAL_DESTINATIONS"); ok {
		cfg.TrustedInternal = strings.Split(v, ",")
	}
	if v, ok := getenv("RELAYDECK_RETENTION_DAYS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RetentionDays = n
		}
	}
	if v, ok := getenv("RELAYDECK_HOSTED"); ok {
		cfg.Hosted = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := getenv("STRIPE_SECRET_KEY"); ok {
		cfg.StripeSecretKey = v
	}
	if v, ok := getenv("STRIPE_WEBHOOK_SECRET"); ok {
		cfg.StripeWebhookSecret = v
	}
	if v, ok := getenv("STRIPE_PRICE_STARTER"); ok {
		cfg.StripePriceStarter = v
	}
	if v, ok := getenv("STRIPE_PRICE_GROWTH"); ok {
		cfg.StripePriceGrowth = v
	}
	if v, ok := getenv("STRIPE_SUCCESS_URL"); ok {
		cfg.StripeSuccessURL = v
	}
	if v, ok := getenv("STRIPE_CANCEL_URL"); ok {
		cfg.StripeCancelURL = v
	}
	if v, ok := getenv("RELAYDECK_CONFIG"); ok && cfg.ConfigFile == "" {
		cfg.ConfigFile = v
	}
}

// Validate checks the effective configuration.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("listen_addr must not be empty")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.MaxPayloadBytes <= 0 || c.MaxPayloadBytes > 64<<20 {
		return fmt.Errorf("max_payload_bytes must be between 1 and %d", 64<<20)
	}
	if c.Workers <= 0 || c.Workers > 64 {
		return fmt.Errorf("workers must be between 1 and 64")
	}
	if c.RetentionDays < 0 {
		return fmt.Errorf("retention_days must be >= 0")
	}
	for _, d := range c.Destinations {
		if strings.TrimSpace(d.Name) == "" {
			return fmt.Errorf("destination with empty name in config")
		}
		if strings.TrimSpace(d.URL) == "" {
			return fmt.Errorf("destination %q: url must not be empty", d.Name)
		}
	}
	for _, s := range c.Sources {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("source with empty name in config")
		}
	}
	return nil
}

// IsTrustedInternal reports whether host is on the explicit trusted list.
func (c *Config) IsTrustedInternal(host string) bool {
	for _, h := range c.TrustedInternal {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(host)) {
			return true
		}
	}
	return false
}
