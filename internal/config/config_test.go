package config

import (
	"testing"

	"nycu-sdc/nexmin/internal/googlegroup"
)

// A nested sub-config must merge field by field. configutil.Merge on its own compares
// the whole struct against its zero value, so a single set field in a later layer
// would replace the entire struct and drop everything set by earlier layers.
func TestMergeConfigPreservesUnsetNestedFields(t *testing.T) {
	base := &Config{
		Port: "8080",
		GoogleGroup: googlegroup.Config{
			ServiceAccountKey:  "key-from-file",
			ImpersonateSubject: "admin@example.com",
			CacheTTL:           "5m",
		},
	}
	override := &Config{
		GoogleGroup: googlegroup.Config{
			ImpersonateSubject: "other-admin@example.com",
		},
	}

	merged, err := mergeConfig(base, override)
	if err != nil {
		t.Fatalf("mergeConfig returned an error: %v", err)
	}

	if merged.GoogleGroup.ImpersonateSubject != "other-admin@example.com" {
		t.Errorf("override did not apply: got %q", merged.GoogleGroup.ImpersonateSubject)
	}
	if merged.GoogleGroup.ServiceAccountKey != "key-from-file" {
		t.Errorf("service account key was lost: got %q", merged.GoogleGroup.ServiceAccountKey)
	}
	if merged.GoogleGroup.CacheTTL != "5m" {
		t.Errorf("cache TTL was lost: got %q", merged.GoogleGroup.CacheTTL)
	}
	if merged.Port != "8080" {
		t.Errorf("top-level field was lost: got %q", merged.Port)
	}
}

func TestMergeConfigOverridesTopLevelFields(t *testing.T) {
	base := &Config{Host: "localhost", Port: "8080"}
	override := &Config{Port: "9090"}

	merged, err := mergeConfig(base, override)
	if err != nil {
		t.Fatalf("mergeConfig returned an error: %v", err)
	}

	if merged.Port != "9090" {
		t.Errorf("got port %q, want 9090", merged.Port)
	}
	if merged.Host != "localhost" {
		t.Errorf("got host %q, want localhost", merged.Host)
	}
}

func TestValidateRequiresDatabaseURL(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error when database_url is empty")
	}

	c.DatabaseURL = "postgres://localhost:5432/db"
	if err := c.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}
