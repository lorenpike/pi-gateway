package config

// config_test.go exercises env parsing for ALL WALLE_* vars (Phase 5 of the
// wall-e gateway plan). Env is driven via t.Setenv so each test is isolated.

import (
	"os"
	"testing"
	"time"
)

// allWalleEnv is the full set of WALLE_* vars Load reads. Tests clear/restore
// these between cases so a stale host environment can't leak in.
var allWalleEnv = []string{
	"WALLE_TOKEN",
	"WALLE_PORT",
	"WALLE_HTTP_QUEUE_TIMEOUT",
	"WALLE_POOL_SIZE",
	"WALLE_DRAIN_TIMEOUT",
	"WALLE_SESSION_DIR",
	"WALLE_PI_BIN",
	"WALLE_PROVIDER",
	"WALLE_MODEL",
	"WALLE_CONFIRM_DEFAULT",
	"WALLE_LOG_LEVEL",
}

// clearWalleEnv unsets every WALLE_* var Load reads, for a known-clean start.
func clearWalleEnv(t *testing.T) {
	t.Helper()
	for _, k := range allWalleEnv {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if cfg.HTTP.Token != "secret" {
		t.Errorf("HTTP.Token = %q, want %q", cfg.HTTP.Token, "secret")
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.QueueTimeout != 60*time.Second {
		t.Errorf("HTTP.QueueTimeout = %v, want 60s", cfg.HTTP.QueueTimeout)
	}
	if cfg.Pool.Size != 4 {
		t.Errorf("Pool.Size = %d, want 4", cfg.Pool.Size)
	}
	if cfg.Pool.DrainTimeout != 30*time.Second {
		t.Errorf("Pool.DrainTimeout = %v, want 30s", cfg.Pool.DrainTimeout)
	}
	if cfg.Session.SessionDir != "/home/wall-e/sessions" {
		t.Errorf("Session.SessionDir = %q, want default", cfg.Session.SessionDir)
	}
	if cfg.SessionDir != "/home/wall-e/sessions" {
		t.Errorf("SessionDir = %q, want default", cfg.SessionDir)
	}
	if cfg.RPC.PiBin != "pi" {
		t.Errorf("RPC.PiBin = %q, want pi", cfg.RPC.PiBin)
	}
	if cfg.RPC.Provider != "" {
		t.Errorf("RPC.Provider = %q, want empty", cfg.RPC.Provider)
	}
	if cfg.RPC.Model != "" {
		t.Errorf("RPC.Model = %q, want empty", cfg.RPC.Model)
	}
	if cfg.RPC.SessionDir != "/home/wall-e/sessions" {
		t.Errorf("RPC.SessionDir = %q, want default", cfg.RPC.SessionDir)
	}
	if !cfg.RPC.UIPolicy.ConfirmedDefault {
		t.Errorf("RPC.UIPolicy.ConfirmedDefault = false, want true")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
}

func TestLoad_RequiredTokenMissing(t *testing.T) {
	clearWalleEnv(t)
	// WALLE_TOKEN deliberately unset.

	_, err := Load()
	if err == nil {
		t.Fatal("Load: expected error for missing WALLE_TOKEN, got nil")
	}
	if !contains(err.Error(), "WALLE_TOKEN is required") {
		t.Errorf("Load: error %q does not mention WALLE_TOKEN is required", err.Error())
	}
}

func TestLoad_EmptyTokenErrors(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: expected error for empty WALLE_TOKEN, got nil")
	}
}

func TestLoad_ExplicitOverrides(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "tok")
	t.Setenv("WALLE_PORT", "9090")
	t.Setenv("WALLE_HTTP_QUEUE_TIMEOUT", "10s")
	t.Setenv("WALLE_POOL_SIZE", "8")
	t.Setenv("WALLE_DRAIN_TIMEOUT", "5m")
	t.Setenv("WALLE_SESSION_DIR", "/tmp/sess")
	t.Setenv("WALLE_PI_BIN", "/usr/local/bin/pi")
	t.Setenv("WALLE_PROVIDER", "openai")
	t.Setenv("WALLE_MODEL", "openai/gpt-5")
	t.Setenv("WALLE_CONFIRM_DEFAULT", "false")
	t.Setenv("WALLE_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q, want :9090", cfg.HTTP.Addr)
	}
	if cfg.HTTP.QueueTimeout != 10*time.Second {
		t.Errorf("HTTP.QueueTimeout = %v, want 10s", cfg.HTTP.QueueTimeout)
	}
	if cfg.Pool.Size != 8 {
		t.Errorf("Pool.Size = %d, want 8", cfg.Pool.Size)
	}
	if cfg.Pool.DrainTimeout != 5*time.Minute {
		t.Errorf("Pool.DrainTimeout = %v, want 5m", cfg.Pool.DrainTimeout)
	}
	if cfg.Session.SessionDir != "/tmp/sess" {
		t.Errorf("Session.SessionDir = %q, want /tmp/sess", cfg.Session.SessionDir)
	}
	if cfg.RPC.PiBin != "/usr/local/bin/pi" {
		t.Errorf("RPC.PiBin = %q", cfg.RPC.PiBin)
	}
	if cfg.RPC.Provider != "openai" {
		t.Errorf("RPC.Provider = %q", cfg.RPC.Provider)
	}
	if cfg.RPC.Model != "openai/gpt-5" {
		t.Errorf("RPC.Model = %q", cfg.RPC.Model)
	}
	if cfg.RPC.UIPolicy.ConfirmedDefault {
		t.Errorf("UIPolicy.ConfirmedDefault = true, want false")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestLoad_DurationParseErrors(t *testing.T) {
	t.Run("bad WALLE_HTTP_QUEUE_TIMEOUT", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_HTTP_QUEUE_TIMEOUT", "not-a-duration")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !contains(err.Error(), "WALLE_HTTP_QUEUE_TIMEOUT") {
			t.Errorf("error %q does not mention WALLE_HTTP_QUEUE_TIMEOUT", err.Error())
		}
	})

	t.Run("bad WALLE_DRAIN_TIMEOUT", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_DRAIN_TIMEOUT", "5")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !contains(err.Error(), "WALLE_DRAIN_TIMEOUT") {
			t.Errorf("error %q does not mention WALLE_DRAIN_TIMEOUT", err.Error())
		}
	})

	t.Run("zero/negative duration rejected", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_DRAIN_TIMEOUT", "0s")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error for 0s duration, got nil")
		}
	})
}

func TestLoad_InvalidPoolSize(t *testing.T) {
	t.Run("non-numeric", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_POOL_SIZE", "many")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !contains(err.Error(), "WALLE_POOL_SIZE") {
			t.Errorf("error %q does not mention WALLE_POOL_SIZE", err.Error())
		}
	})

	t.Run("zero", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_POOL_SIZE", "0")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error for pool size 0, got nil")
		}
		if !contains(err.Error(), "WALLE_POOL_SIZE") {
			t.Errorf("error %q does not mention WALLE_POOL_SIZE", err.Error())
		}
	})
}

func TestLoad_ConfirmDefaultBoolParsing(t *testing.T) {
	cases := []struct{ in, name string; want bool }{
		{"false", "false", false},
		{"FALSE", "FALSE", false},
		{"0", "0", false},
		{"no", "no", false},
		{"off", "off", false},
		{"true", "true", true},
		{"YES", "YES", true},
		{"1", "1", true},
		{"on", "on", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearWalleEnv(t)
			t.Setenv("WALLE_TOKEN", "x")
			t.Setenv("WALLE_CONFIRM_DEFAULT", c.in)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RPC.UIPolicy.ConfirmedDefault != c.want {
				t.Errorf("CONFIRM_DEFAULT=%q → %v, want %v", c.in, cfg.RPC.UIPolicy.ConfirmedDefault, c.want)
			}
		})
	}

	t.Run("invalid bool", func(t *testing.T) {
		clearWalleEnv(t)
		t.Setenv("WALLE_TOKEN", "x")
		t.Setenv("WALLE_CONFIRM_DEFAULT", "maybe")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !contains(err.Error(), "WALLE_CONFIRM_DEFAULT") {
			t.Errorf("error %q does not mention WALLE_CONFIRM_DEFAULT", err.Error())
		}
	})
}

func TestLoad_InvalidPort(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "x")
	t.Setenv("WALLE_PORT", "not-a-port")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "WALLE_PORT") {
		t.Errorf("error %q does not mention WALLE_PORT", err.Error())
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "x")
	t.Setenv("WALLE_LOG_LEVEL", "trace")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "WALLE_LOG_LEVEL") {
		t.Errorf("error %q does not mention WALLE_LOG_LEVEL", err.Error())
	}
}

func TestLoad_MultipleErrorsAllReported(t *testing.T) {
	clearWalleEnv(t)
	// No token, plus several bad values.
	t.Setenv("WALLE_DRAIN_TIMEOUT", "nope")
	t.Setenv("WALLE_POOL_SIZE", "0")
	t.Setenv("WALLE_LOG_LEVEL", "trace")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	// Every bad var should be mentioned, so a user fixes them all at once.
	for _, want := range []string{"WALLE_TOKEN", "WALLE_DRAIN_TIMEOUT", "WALLE_POOL_SIZE", "WALLE_LOG_LEVEL"} {
		if !contains(msg, want) {
			t.Errorf("error %q does not mention %s", msg, want)
		}
	}
}

// contains is a tiny strings.Contains so the test file doesn't need to import
// the strings package just for one check.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
