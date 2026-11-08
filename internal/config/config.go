package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvProduction  = "production"

	// ExampleJWTSecret and ExamplePaystackSecret are the values shipped in .env.example.
	// Production refuses to start with them.
	ExampleJWTSecret      = "dev-only-change-me-use-32chars-min!!"
	ExamplePaystackSecret = "dev-paystack-secret"
)

type Config struct {
	Env            string
	DatabaseURL    string
	HTTPAddr       string
	JWTSecret      string
	PaystackSecret string
	Seed           bool
	SeedPassword   string
	HoldMinutes    int
	TokenTTL       time.Duration
	CORSOrigins    []string
	// TrustProxy makes the login rate limiter use the last X-Forwarded-For entry
	// (the one appended by your own reverse proxy) instead of the socket address.
	TrustProxy bool
	// Login rate limits per minute. Zero means the default (30 per IP, 6 per email).
	LoginRatePerIP    int
	LoginRatePerEmail int
}

func FromEnv() (Config, error) {
	hold := 15
	if v := strings.TrimSpace(os.Getenv("TILL_HOLD_MINUTES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 24*60 {
			return Config{}, fmt.Errorf("TILL_HOLD_MINUTES must be between 1 and 1440")
		}
		hold = n
	}
	ttl := 8 * time.Hour
	if v := strings.TrimSpace(os.Getenv("TILL_TOKEN_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 30*24*time.Hour {
			return Config{}, fmt.Errorf("TILL_TOKEN_TTL must be a duration between 1m and 720h, such as 8h")
		}
		ttl = d
	}
	c := Config{
		Env:               strings.ToLower(strings.TrimSpace(getenv("TILL_ENV", EnvDevelopment))),
		DatabaseURL:       os.Getenv("TILL_DATABASE_URL"),
		HTTPAddr:          getenv("TILL_HTTP_ADDR", ":8080"),
		JWTSecret:         os.Getenv("TILL_JWT_SECRET"),
		PaystackSecret:    os.Getenv("TILL_PAYSTACK_SECRET"),
		Seed:              boolEnv("TILL_SEED", false),
		SeedPassword:      getenv("TILL_SEED_PASSWORD", "change-me-now-12"),
		HoldMinutes:       hold,
		TokenTTL:          ttl,
		CORSOrigins:       splitList(os.Getenv("TILL_CORS_ORIGINS")),
		TrustProxy:        boolEnv("TILL_TRUST_PROXY", false),
		LoginRatePerIP:    intEnv("TILL_LOGIN_RATE_IP"),
		LoginRatePerEmail: intEnv("TILL_LOGIN_RATE_EMAIL"),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks the config. In production it also refuses development defaults.
func (c Config) Validate() error {
	if c.Env != EnvDevelopment && c.Env != EnvProduction {
		return fmt.Errorf("TILL_ENV must be development or production")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("TILL_DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("TILL_JWT_SECRET must be at least 32 characters")
	}
	if len(c.PaystackSecret) < 16 {
		return fmt.Errorf("TILL_PAYSTACK_SECRET must be at least 16 characters")
	}
	if c.Seed && len(c.SeedPassword) < 12 {
		return fmt.Errorf("TILL_SEED_PASSWORD must be at least 12 characters")
	}
	if c.Env == EnvProduction {
		if c.JWTSecret == ExampleJWTSecret || strings.Contains(c.JWTSecret, "dev-only") {
			return fmt.Errorf("production: TILL_JWT_SECRET is a development value")
		}
		if c.PaystackSecret == ExamplePaystackSecret || strings.HasPrefix(c.PaystackSecret, "dev-") {
			return fmt.Errorf("production: TILL_PAYSTACK_SECRET is a development value")
		}
		if len(c.JWTSecret) < 48 {
			return fmt.Errorf("production: TILL_JWT_SECRET must be at least 48 characters")
		}
		if len(c.PaystackSecret) < 24 {
			return fmt.Errorf("production: TILL_PAYSTACK_SECRET must be at least 24 characters")
		}
		if c.Seed {
			return fmt.Errorf("production: TILL_SEED must not be true")
		}
		for _, o := range c.CORSOrigins {
			if o == "*" {
				return fmt.Errorf("production: TILL_CORS_ORIGINS must list explicit origins, not *")
			}
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.TrimRight(p, "/"))
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolEnv(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func intEnv(k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
