package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

type Config struct {
	Env             string
	Concurrency     int
	MaxSourceBytes  int64
	JobTimeout      time.Duration
	ShutdownTimeout time.Duration
	MetricsAddr     string

	RabbitURL string
	S3        S3Config
}

type S3Config struct {
	Endpoint       string
	PublicEndpoint string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	UsePathStyle   bool
	PresignTTL     time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Env:             env("APP_ENV", "development"),
		Concurrency:     envInt("WORKER_CONCURRENCY", defaultConcurrency()),
		MaxSourceBytes:  envInt64("MAX_SOURCE_BYTES", 50<<20),
		JobTimeout:      envDuration("JOB_TIMEOUT", 2*time.Minute),
		ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 30*time.Second),
		MetricsAddr:     env("METRICS_ADDR", ":8090"),
		RabbitURL:       env("RABBITMQ_URL", ""),
		S3: S3Config{
			Endpoint:       env("S3_ENDPOINT", ""),
			PublicEndpoint: env("S3_PUBLIC_ENDPOINT", ""),
			Region:         env("S3_REGION", "us-east-1"),
			Bucket:         env("S3_BUCKET", ""),
			AccessKey:      env("S3_ACCESS_KEY", ""),
			SecretKey:      env("S3_SECRET_KEY", ""),
			UsePathStyle:   envBool("S3_USE_PATH_STYLE", false),
			PresignTTL:     envDuration("S3_PRESIGN_TTL", time.Hour),
		},
	}

	for key, value := range map[string]string{
		"RABBITMQ_URL":  cfg.RabbitURL,
		"S3_BUCKET":     cfg.S3.Bucket,
		"S3_ACCESS_KEY": cfg.S3.AccessKey,
		"S3_SECRET_KEY": cfg.S3.SecretKey,
	} {
		if value == "" {
			return Config{}, fmt.Errorf("config: %s is required", key)
		}
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return cfg, nil
}

// defaultConcurrency keeps one in-flight job per core; image encoding is CPU
// bound, so oversubscribing only adds latency.
func defaultConcurrency() int {
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 2
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(env(key, ""))
	if err != nil {
		return fallback
	}
	return v
}

func envInt64(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(env(key, ""))
	if err != nil {
		return fallback
	}
	return v
}
