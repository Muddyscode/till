package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL    string
	HTTPAddr       string
	JWTSecret      string
	PaystackSecret string
	Seed           bool
	SeedPassword   string
	HoldMinutes    int
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
	c := Config{
		DatabaseURL:    os.Getenv("TILL_DATABASE_URL"),
		HTTPAddr:       getenv("TILL_HTTP_ADDR", ":8080"),
		JWTSecret:      os.Getenv("TILL_JWT_SECRET"),
		PaystackSecret: os.Getenv("TILL_PAYSTACK_SECRET"),
		Seed:           boolEnv("TILL_SEED", false),
		SeedPassword:   getenv("TILL_SEED_PASSWORD", "change-me-now-12"),
		HoldMinutes:    hold,
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("TILL_DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("TILL_JWT_SECRET must be at least 32 characters")
	}
	if len(c.PaystackSecret) < 16 {
		return Config{}, fmt.Errorf("TILL_PAYSTACK_SECRET must be at least 16 characters")
	}
	if c.Seed && len(c.SeedPassword) < 12 {
		return Config{}, fmt.Errorf("TILL_SEED_PASSWORD must be at least 12 characters")
	}
	return c, nil
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
