package main

import (
	"bytes"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
)

// ---------------------------------------------------------------------------
// parseBytes
// ---------------------------------------------------------------------------

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"512", 512},
		{"512B", 512},
		{"1kb", 1024},
		{"1KB", 1024},
		{"256MB", 256 << 20},
		{" 2 GB ", 2 << 30},
		{"8589934591GB", 8589934591 << 30}, // largest GB value that fits in int64
	}
	for _, tc := range cases {
		got, err := parseBytes(tc.in)
		if err != nil {
			t.Errorf("parseBytes(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseBytesErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"abc",
		"-1MB",
		"1.5GB",
		"1TB",
		"9999999999GB",         // OPS-010: overflows int64 after multiplication
		"8589934592GB",         // exactly 2^63 bytes
		"9007199254740992MB",   // 2^53 MB overflows
		"99999999999999999999", // does not fit in int64 at all
	} {
		got, err := parseBytes(in)
		if err == nil {
			t.Errorf("parseBytes(%q) = %d, want error", in, got)
		}
		if got < 0 {
			t.Errorf("parseBytes(%q) returned negative %d", in, got)
		}
	}
	if math.MaxInt64/(1<<30) != 8589934591 {
		t.Fatal("test table assumption about MaxInt64/GB is wrong")
	}
}

// ---------------------------------------------------------------------------
// applyFlagOverrides precedence
// ---------------------------------------------------------------------------

func TestApplyFlagOverrides_ZeroValuesKeepConfig(t *testing.T) {
	cfg := mustDefaults(t)
	want := *cfg
	if err := applyFlagOverrides(cfg, flagOverrides{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != want.Server.Addr || cfg.Files.Root != want.Files.Root ||
		cfg.Cache.Enabled != want.Cache.Enabled || cfg.Cache.MaxBytes != want.Cache.MaxBytes {
		t.Errorf("empty overrides changed config: %+v", cfg)
	}
}

func TestApplyFlagOverrides_FlagsWinOverEnv(t *testing.T) {
	t.Setenv("STATIC_SERVER_ADDR", "10.0.0.1:7000")
	t.Setenv("STATIC_FILES_ROOT", "/env/root")
	t.Setenv("STATIC_CACHE_MAX_BYTES", "1024")
	t.Setenv("STATIC_SECURITY_CSP", "env-csp")

	cfg := mustDefaults(t)
	err := applyFlagOverrides(cfg, flagOverrides{
		root:           "/cli/root",
		port:           9000,
		cacheSize:      "1MB",
		csp:            "cli-csp",
		cors:           " https://a.example , https://b.example ",
		tlsPort:        9443,
		noCompress:     true,
		noEtag:         true,
		noDotfileBlock: true,
		dirListing:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Port from CLI, host preserved from env.
	if cfg.Server.Addr != "10.0.0.1:9000" {
		t.Errorf("Addr = %q, want 10.0.0.1:9000", cfg.Server.Addr)
	}
	if cfg.Server.TLSAddr != ":9443" {
		t.Errorf("TLSAddr = %q, want :9443", cfg.Server.TLSAddr)
	}
	if cfg.Files.Root != "/cli/root" {
		t.Errorf("Root = %q, want /cli/root", cfg.Files.Root)
	}
	if cfg.Cache.MaxBytes != 1<<20 {
		t.Errorf("MaxBytes = %d, want %d", cfg.Cache.MaxBytes, 1<<20)
	}
	if cfg.Security.CSP != "cli-csp" {
		t.Errorf("CSP = %q, want cli-csp", cfg.Security.CSP)
	}
	if got := cfg.Security.CORSOrigins; len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Errorf("CORSOrigins = %q", got)
	}
	if cfg.Compression.Enabled || cfg.Headers.EnableETags || cfg.Security.BlockDotfiles || !cfg.Security.DirectoryListing {
		t.Errorf("boolean flags not applied: %+v %+v %+v", cfg.Compression, cfg.Headers, cfg.Security)
	}
}

func TestApplyFlagOverrides_HostOnlyKeepsPort(t *testing.T) {
	cfg := mustDefaults(t)
	if err := applyFlagOverrides(cfg, flagOverrides{host: "::1"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != "[::1]:8080" {
		t.Errorf("Addr = %q, want [::1]:8080", cfg.Server.Addr)
	}
}

func TestApplyFlagOverrides_BadCacheSize(t *testing.T) {
	cfg := mustDefaults(t)
	if err := applyFlagOverrides(cfg, flagOverrides{cacheSize: "9999999999GB"}); err == nil {
		t.Error("expected error for overflowing --cache-size")
	}
}

// ---------------------------------------------------------------------------
// loadConfig: explicit vs implicit --config, validation
// ---------------------------------------------------------------------------

func TestLoadConfig_ImplicitMissingDefaultIsOK(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.toml")
	if _, err := loadConfig(missing, false, flagOverrides{}); err != nil {
		t.Fatalf("implicit missing default should be ignored, got %v", err)
	}
}

// OPS-006
func TestLoadConfig_ExplicitMissingIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	_, err := loadConfig(missing, true, flagOverrides{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found error", err)
	}
}

// OPS-007
func TestLoadConfig_ValidatesAfterOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("[compression]\nlevel = 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, true, flagOverrides{}); err == nil {
		t.Fatal("expected validation error for compression.level = 42")
	}
	// --no-compress makes the level irrelevant.
	if _, err := loadConfig(path, true, flagOverrides{noCompress: true}); err != nil {
		t.Fatalf("unexpected error with --no-compress: %v", err)
	}
}

// OPS-009: the reload path must re-apply CLI overrides and the positional root.
func TestLoadConfig_ReloadKeepsOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("[files]\nroot = \"/file/root\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := flagOverrides{root: "/cli/root", port: 9999}
	for i := 0; i < 2; i++ { // startup, then a SIGHUP reload
		cfg, err := loadConfig(path, true, f)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Files.Root != "/cli/root" || cfg.Server.Addr != ":9999" {
			t.Fatalf("pass %d: root=%q addr=%q, want CLI values", i, cfg.Files.Root, cfg.Server.Addr)
		}
	}
}

// ---------------------------------------------------------------------------
// serve: startup failures exit non-zero (OPS-001)
// ---------------------------------------------------------------------------

func TestServe_PortInUseReturnsNonZero(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, port, _ := net.SplitHostPort(occupied.Addr().String())

	code := serve([]string{"-q", "--host", "127.0.0.1", "--port", port, "--no-cache", t.TempDir()})
	if code == 0 {
		t.Fatal("serve returned 0 for an occupied port")
	}
}

func TestServe_MissingExplicitConfigReturnsNonZero(t *testing.T) {
	code := serve([]string{"-q", "--config", filepath.Join(t.TempDir(), "missing.toml")})
	if code != 1 {
		t.Fatalf("serve returned %d, want 1", code)
	}
}

func TestServe_InvalidFlagValueReturnsNonZero(t *testing.T) {
	code := serve([]string{"-q", "--cache-size", "lots"})
	if code != 1 {
		t.Fatalf("serve returned %d, want 1", code)
	}
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// OPS-011: helpText is printed with fmt.Print, so it must not contain
// printf escapes.
func TestHelpTextHasNoPrintfEscapes(t *testing.T) {
	if strings.Contains(helpText, "%%") {
		t.Error("helpText contains %% but is printed with fmt.Print")
	}
}

// OPS-008: the embedded config used by `static-web init` must match the
// documented root config.toml.example.
func TestEmbeddedConfigMatchesRootExample(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", "config.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, configExample) {
		t.Error("cmd/static-web/config.toml.example differs from ./config.toml.example; copy the root file over the embedded one")
	}
}

// The embedded example must itself be a valid configuration.
func TestEmbeddedConfigIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, configExample, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, true, flagOverrides{}); err != nil {
		t.Fatalf("embedded config.toml.example is invalid: %v", err)
	}
}

func mustDefaults(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
