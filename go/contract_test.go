package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistrationPublishesBothPortals(t *testing.T) {
	reg, ok := registration().(map[string]any)
	if !ok {
		t.Fatal("registration is not a map")
	}
	raw, _ := json.Marshal(reg["metadata"])
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["Name"] != "OMC Trae OAuth" {
		t.Fatalf("name = %v", metadata["Name"])
	}
	if metadata["Version"] != pluginVersion {
		t.Fatalf("version = %v, want %v", metadata["Version"], pluginVersion)
	}
	if metadata["GitHubRepository"] != "https://github.com/omc-community/omc-trae-oauth" {
		t.Fatalf("repository = %v", metadata["GitHubRepository"])
	}
	if metadata["Logo"] != publicAssetBaseURL+pluginLogo {
		t.Fatalf("logo = %v", metadata["Logo"])
	}
	caps := reg["capabilities"].(map[string]any)
	for _, key := range []string{"auth_provider", "model_provider", "executor", "quota_provider", "management_api"} {
		if caps[key] != true {
			t.Fatalf("capability %s missing", key)
		}
	}
	entries := authEntries()
	if len(entries) != 2 || entries[0].Identifier != siteTrae || entries[1].Identifier != siteTraeCN {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Logo != publicAssetBaseURL+traeMonoLogo || entries[1].Logo != publicAssetBaseURL+traeColorLogo {
		t.Fatalf("entry logos = %#v", entries)
	}
}

func TestDescriptorMatchesVersion(t *testing.T) {
	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		PluginID      string `json:"plugin_id"`
		PluginVersion string `json:"plugin_version"`
	}
	if err := json.Unmarshal([]byte(omcDescriptorJSON), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || doc.PluginID != pluginStoreID || doc.PluginVersion != pluginVersion {
		t.Fatalf("descriptor = %#v", doc)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	cfg := getConfig()
	if cfg.Site != siteTrae || cfg.MaxConcurrency != 3 || cfg.ClientVersion != defaultClientVersion || cfg.MaxTokens != 32000 {
		t.Fatalf("defaults = %#v", cfg)
	}
	for _, bad := range []string{
		"site: other",
		"site: trae\nmax_concurrency: 0",
		"site: trae\nclient_version: 'bad version'",
		"site: trae\nmax_tokens: 0",
	} {
		if err := configure([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid config %q", bad)
		}
	}
	if err := configure([]byte("site: trae-cn\nmax_concurrency: 5\nclient_version: 0.1.70\nmax_tokens: 64000")); err != nil {
		t.Fatal(err)
	}
	cfg = getConfig()
	if cfg.Site != siteTraeCN || cfg.MaxConcurrency != 5 || cfg.ClientVersion != "0.1.70" || cfg.MaxTokens != 64000 {
		t.Fatalf("configured = %#v", cfg)
	}
}

func TestEmbeddedAssetsMatchPublished(t *testing.T) {
	for _, name := range []string{pluginLogo, traeColorLogo, traeMonoLogo} {
		published, err := os.ReadFile(filepath.Join("..", "assets", name))
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := os.ReadFile(filepath.Join("assets", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, embedded) {
			t.Fatalf("%s differs between assets/ and go/assets/", name)
		}
	}
}
