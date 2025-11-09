package springg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config represents the application configuration
type Config struct {
	Port                int           `json:"port"`
	JWTSecret           string        `json:"jwt_secret"`
	RequireJWT          bool          `json:"require_jwt"`
	Dimensions          int           `json:"dimensions"`
	DataPath            string        `json:"data_path"`
	LogLevel            string        `json:"log_level"`
	MaxVectorsPerIndex  int           `json:"max_vectors_per_index"`
	PersistenceInterval string        `json:"persistence_interval"`
	PersistenceDuration time.Duration `json:"-"`

	// S3 backup settings
	S3Bucket       string        `json:"s3_bucket,omitempty"`
	S3Region       string        `json:"s3_region,omitempty"`
	S3SyncInterval string        `json:"s3_sync_interval,omitempty"`
	S3SyncDuration time.Duration `json:"-"`
	S3Enabled      bool          `json:"-"` // Computed: true if S3Bucket is set

	// Cluster settings
	ClusterEnabled  bool `json:"cluster_enabled"`
	ClusterBindPort int  `json:"cluster_bind_port"`

	// Memory management
	MaxMemoryMB int `json:"max_memory_mb"` // Maximum memory in MB (0 = unlimited)

	// Bulk indexing limits
	MaxConcurrentBulkRequests int     `json:"max_concurrent_bulk_requests"` // Max concurrent bulk requests (0 = unlimited)
	BulkCPUThresholdPercent   float64 `json:"bulk_cpu_threshold_percent"`   // Reject bulk requests if CPU > this % (0 = no limit)
}

// DefaultConfig returns a configuration with default values
func DefaultConfig() *Config {
	return &Config{
		Port:                8080,
		JWTSecret:           "change-this-secret-in-production",
		RequireJWT:          true,
		Dimensions:          1024,
		DataPath:            filepath.Join(os.TempDir(), "springg_data"),
		LogLevel:            "info",
		MaxVectorsPerIndex:  100000,
		PersistenceInterval: "5m",
		PersistenceDuration: 5 * time.Minute,
		S3Bucket:            "",
		S3Region:            "us-east-1",
		S3SyncInterval:      "5m",
		S3SyncDuration:      5 * time.Minute,
		S3Enabled:                 false,
		ClusterEnabled:            true,
		ClusterBindPort:           7946,
		MaxMemoryMB:               2048, // 2GB default
		MaxConcurrentBulkRequests: 2,    // Max 2 concurrent bulk requests
		BulkCPUThresholdPercent:   80.0, // Reject if CPU > 80%
	}
}

// LoadConfig loads configuration from file
func LoadConfig() (*Config, error) {
	// Search order: current directory, then home directory
	configPaths := []string{
		"./springg.json",
		filepath.Join(os.Getenv("HOME"), "springg.json"),
	}

	var configPath string
	for _, path := range configPaths {
		if _, err := os.Stat(path); err == nil {
			configPath = path
			break
		}
	}

	// If no config found, create default config
	if configPath == "" {
		config := DefaultConfig()
		if err := SaveConfig(config, "./springg.json"); err != nil {
			return nil, fmt.Errorf("failed to create default config: %w", err)
		}
		fmt.Println("Created default configuration file: ./springg.json")
		return config, nil
	}

	// Load existing config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Parse persistence interval
	if config.PersistenceInterval != "" {
		duration, err := time.ParseDuration(config.PersistenceInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid persistence_interval: %w", err)
		}
		config.PersistenceDuration = duration
	}

	// Parse S3 sync interval
	if config.S3SyncInterval != "" {
		duration, err := time.ParseDuration(config.S3SyncInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid s3_sync_interval: %w", err)
		}
		config.S3SyncDuration = duration
	}

	// Enable S3 if bucket is configured
	config.S3Enabled = config.S3Bucket != ""

	// Validate required fields
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// SaveConfig saves configuration to file
func SaveConfig(config *Config, path string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}

	if c.RequireJWT && c.JWTSecret == "" {
		return fmt.Errorf("jwt_secret is required when require_jwt is true")
	}

	if c.Dimensions <= 0 {
		return fmt.Errorf("dimensions must be positive: %d", c.Dimensions)
	}

	if c.DataPath == "" {
		return fmt.Errorf("data_path is required")
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log_level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	return nil
}
