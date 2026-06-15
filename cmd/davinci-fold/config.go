package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/vocdoni/davinci-fold/internal"
)

const (
	defaultAPIHost          = "0.0.0.0"
	defaultAPIPort          = 8888
	defaultLogLevel         = "info"
	defaultLogOutput        = "stdout"
	defaultDatadir          = ".davinci-fold" // Prefixed with the user's home directory.
	defaultBatchSize        = 64
	defaultBatchTimeWindow  = 5 * time.Minute
	defaultFoldEvery        = 4
	defaultWorkerPollPeriod = 10 * time.Second
)

// Version is the build version, set at build time with -ldflags.
var Version = internal.Version

// Config holds the davinci-fold orchestrator configuration.
type Config struct {
	API     APIConfig
	Batch   BatchConfig
	Fold    FoldConfig
	Worker  WorkerConfig
	Log     LogConfig
	Datadir string `mapstructure:"datadir"`
}

// APIConfig holds the HTTP API configuration.
type APIConfig struct {
	Host      string `mapstructure:"host"`      // API host address
	Port      int    `mapstructure:"port"`      // API port number
	JWTSecret string `mapstructure:"jwtSecret"` // HMAC secret for admin/keywarden JWTs
}

// BatchConfig controls when a batch of votes seals.
type BatchConfig struct {
	Size int           `mapstructure:"size"` // Seal a batch once this many votes accumulate
	Time time.Duration `mapstructure:"time"` // Or once this much time elapses since the first vote
}

// FoldConfig controls the fold cadence.
type FoldConfig struct {
	Every int `mapstructure:"every"` // Fold after this many imported batch STARKs
}

// WorkerConfig holds the prover-worker pool configuration.
type WorkerConfig struct {
	PollPeriod time.Duration `mapstructure:"pollPeriod"` // Health-poll interval per worker
}

// LogConfig holds logging configuration.
type LogConfig struct {
	Level      string `mapstructure:"level"`
	Output     string `mapstructure:"output"`
	DisableAPI bool   `mapstructure:"disableAPI"` // Disable API logging middleware
}

// loadConfig loads configuration from flags, environment variables and defaults.
func loadConfig() (*Config, error) {
	cfg := &Config{}

	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		userHomeDir = "."
	}
	defaultDatadirPath := filepath.Join(userHomeDir, defaultDatadir)

	// API
	flag.StringP("api.host", "h", defaultAPIHost, "API host")
	flag.IntP("api.port", "p", defaultAPIPort, "API port")
	flag.String("api.jwtSecret", "", "HMAC secret for admin/keywarden JWT authentication (required)")
	// Batch sealing
	flag.Int("batch.size", defaultBatchSize, "seal a batch once this many votes accumulate")
	flag.DurationP("batch.time", "b", defaultBatchTimeWindow, "seal a batch once this much time elapses (i.e 5m or 1h)")
	// Fold cadence
	flag.Int("fold.every", defaultFoldEvery, "fold after this many imported batch STARKs")
	// Worker pool
	flag.Duration("worker.pollPeriod", defaultWorkerPollPeriod, "health-poll interval per prover worker")
	// Logging
	flag.StringP("log.level", "l", defaultLogLevel, "log level (debug, info, warn, error, fatal)")
	flag.StringP("log.output", "o", defaultLogOutput, "log output (stdout, stderr or filepath)")
	flag.Bool("log.disableAPI", false, "disable API logging middleware")
	// Storage
	flag.StringP("datadir", "d", defaultDatadirPath, "data directory for database and storage files")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "davinci-fold v%s\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage: davinci-fold [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment variables are also available with the same name as flags,\n")
		fmt.Fprintf(os.Stderr, "  prefixed with DAVINCIFOLD_ and with dashes (-) and dots (.) replaced by underscores (_).\n")
		fmt.Fprintf(os.Stderr, "  For example, DAVINCIFOLD_API_PORT or DAVINCIFOLD_API_JWTSECRET\n\n")
	}

	flag.CommandLine.SortFlags = false
	flag.Parse()

	viper.SetEnvPrefix("DAVINCIFOLD")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.BindPFlags(flag.CommandLine); err != nil {
		return nil, fmt.Errorf("error binding flags: %w", err)
	}
	if err := viper.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	return cfg, nil
}

// validateConfig validates the loaded configuration.
func validateConfig(cfg *Config) error {
	if cfg.API.JWTSecret == "" {
		return fmt.Errorf("JWT secret is required (use --api.jwtSecret flag or DAVINCIFOLD_API_JWTSECRET environment variable)")
	}
	if cfg.Batch.Size < 2 {
		return fmt.Errorf("batch size must be at least 2, got: %d", cfg.Batch.Size)
	}
	if cfg.Batch.Size > 256 {
		return fmt.Errorf("batch size must not exceed 256 (circuit MAX_BATCH_SIZE), got: %d", cfg.Batch.Size)
	}
	if cfg.Fold.Every < 1 {
		return fmt.Errorf("fold cadence must be at least 1, got: %d", cfg.Fold.Every)
	}
	return nil
}
