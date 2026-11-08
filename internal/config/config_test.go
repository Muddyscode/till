package config

import (
	"strings"
	"testing"
	"time"
)

func goodProd() Config {
	return Config{
		Env: EnvProduction, DatabaseURL: "postgres://x", HTTPAddr: ":8080",
		JWTSecret:      strings.Repeat("k", 48),
		PaystackSecret: "sk_live_" + strings.Repeat("a", 24),
		TokenTTL:       time.Hour,
	}
}

func TestProductionValidation(t *testing.T) {
	if err := goodProd().Validate(); err != nil {
		t.Fatalf("good prod config rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"example jwt":      func(c *Config) { c.JWTSecret = ExampleJWTSecret },
		"example paystack": func(c *Config) { c.PaystackSecret = ExamplePaystackSecret },
		"short jwt":        func(c *Config) { c.JWTSecret = strings.Repeat("k", 40) },
		"short paystack":   func(c *Config) { c.PaystackSecret = strings.Repeat("p", 20) },
		"seed":             func(c *Config) { c.Seed = true; c.SeedPassword = "long-enough-pass-1" },
		"cors star":        func(c *Config) { c.CORSOrigins = []string{"*"} },
		"bad env":          func(c *Config) { c.Env = "staging" },
	}
	for name, mut := range cases {
		c := goodProd()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDevelopmentAllowsExamples(t *testing.T) {
	c := Config{Env: EnvDevelopment, DatabaseURL: "postgres://x", JWTSecret: ExampleJWTSecret,
		PaystackSecret: ExamplePaystackSecret, Seed: true, SeedPassword: "change-me-now-12"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("TILL_DATABASE_URL", "postgres://x")
	t.Setenv("TILL_JWT_SECRET", ExampleJWTSecret)
	t.Setenv("TILL_PAYSTACK_SECRET", ExamplePaystackSecret)
	t.Setenv("TILL_ENV", "")
	t.Setenv("TILL_TOKEN_TTL", "")
	t.Setenv("TILL_CORS_ORIGINS", "https://a.example/, https://b.example")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != EnvDevelopment || c.TokenTTL != 8*time.Hour {
		t.Fatalf("defaults: %+v", c)
	}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "https://a.example" {
		t.Fatalf("cors: %v", c.CORSOrigins)
	}
	t.Setenv("TILL_ENV", "production")
	if _, err := FromEnv(); err == nil {
		t.Fatal("production with example secrets must fail")
	}
	t.Setenv("TILL_ENV", "development")
	t.Setenv("TILL_TOKEN_TTL", "30s")
	if _, err := FromEnv(); err == nil {
		t.Fatal("ttl below 1m must fail")
	}
	t.Setenv("TILL_TOKEN_TTL", "2h")
	if c, err = FromEnv(); err != nil || c.TokenTTL != 2*time.Hour {
		t.Fatalf("ttl: %v %v", c.TokenTTL, err)
	}
}
