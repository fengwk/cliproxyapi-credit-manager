package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

const (
	expectedPluginID   = "cliproxyapi-credit-manager"
	expectedPluginName = "Credit Manager"
	expectedRepository = "https://github.com/fengwk/cliproxyapi-credit-manager"
)

// modulePath extracts the module declaration from go.mod.
func modulePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

// TestCustomRegistryUsesCPAStoreContract validates registry.json against CPA's
// official pluginstore parser and validator, ensuring the custom store source
// contract remains unbroken across releases and aligned with config.example.yaml.
func TestCustomRegistryUsesCPAStoreContract(t *testing.T) {
	module := modulePath(t)
	if module != "github.com/fengwk/cliproxyapi-credit-manager" {
		t.Fatalf("unexpected module path %q", module)
	}

	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/registry.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	sourceURL := server.URL + "/registry.json"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := pluginstore.NewClient(server.Client(), sourceURL)
	registry, err := client.FetchRegistry(ctx)
	if err != nil {
		t.Fatalf("CPA rejected registry: %v", err)
	}

	if registry.SchemaVersion != pluginstore.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", registry.SchemaVersion, pluginstore.SchemaVersion)
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("registry contains %d plugins, want 1", len(registry.Plugins))
	}

	plugin := registry.Plugins[0]
	if plugin.ID != expectedPluginID {
		t.Fatalf("plugin ID = %q, want %q", plugin.ID, expectedPluginID)
	}
	if plugin.Name != expectedPluginName {
		t.Fatalf("plugin Name = %q, want %q", plugin.Name, expectedPluginName)
	}
	if plugin.Name == plugin.ID {
		t.Fatalf("plugin Name %q must differ from technical ID %q", plugin.Name, plugin.ID)
	}
	if plugin.Repository != expectedRepository || plugin.Repository != "https://"+module {
		t.Fatalf("repository = %q, want %q", plugin.Repository, expectedRepository)
	}
	if pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease {
		t.Fatalf("install.type = %q, want %q", pluginstore.PluginInstallType(plugin), pluginstore.InstallTypeGitHubRelease)
	}
	if plugin.Version != "" {
		t.Fatalf("registry must not pin version %q; store resolves latest release dynamically", plugin.Version)
	}
	if plugin.Author == "" || plugin.License == "" || plugin.Description == "" || len(plugin.Tags) == 0 {
		t.Fatalf("missing required metadata in plugin: %#v", plugin)
	}

	// Ensure no version key is present in loose JSON representation
	var loose map[string]any
	if err := json.Unmarshal(data, &loose); err != nil {
		t.Fatalf("decode loose JSON: %v", err)
	}
	plugins, ok := loose["plugins"].([]any)
	if !ok || len(plugins) != 1 {
		t.Fatalf("unexpected plugins array in loose JSON: %#v", loose["plugins"])
	}
	pluginMap, ok := plugins[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected plugin entry: %#v", plugins[0])
	}
	if _, hasVersion := pluginMap["version"]; hasVersion {
		t.Fatal("registry must not contain 'version' field; store dynamically resolves releases")
	}

	sources, err := pluginstore.NormalizeSources([]string{sourceURL})
	if err != nil {
		t.Fatalf("normalize sources: %v", err)
	}
	if len(sources) != 2 || sources[0].ID != pluginstore.DefaultSourceID || sources[1].URL != sourceURL {
		t.Fatalf("custom source normalization failed: %#v", sources)
	}

	// Verify config.example.yaml references this exact source URL and configures the plugin.
	var example struct {
		Plugins struct {
			Configs      map[string]any `yaml:"configs"`
			StoreSources []string       `yaml:"store-sources"`
		} `yaml:"plugins"`
	}
	exampleData, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	if err := yaml.Unmarshal(exampleData, &example); err != nil {
		t.Fatalf("unmarshal config.example.yaml: %v", err)
	}
	if _, configured := example.Plugins.Configs[expectedPluginID]; !configured {
		t.Fatalf("config.example.yaml does not configure %q", expectedPluginID)
	}
	expectedSource := "https://raw.githubusercontent.com/" +
		strings.TrimPrefix(module, "github.com/") + "/main/registry.json"
	if len(example.Plugins.StoreSources) != 1 || example.Plugins.StoreSources[0] != expectedSource {
		t.Fatalf("config.example.yaml store-sources = %v, want [%q]", example.Plugins.StoreSources, expectedSource)
	}
}

// TestCustomRegistryRejectsInvalidMutations checks that corrupting required registry fields
// or attempting to pin versions is properly caught by CPA or the contract checks.
func TestCustomRegistryRejectsInvalidMutations(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}

	fetchWithData := func(t *testing.T, payload []byte) (pluginstore.Registry, error) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return pluginstore.NewClient(server.Client(), server.URL).FetchRegistry(ctx)
	}

	// 1. Pinned version must be rejected by CPA client.
	var pinned map[string]any
	if err := json.Unmarshal(data, &pinned); err != nil {
		t.Fatal(err)
	}
	pinned["plugins"].([]any)[0].(map[string]any)["version"] = "v0.1.2"
	pinnedBytes, _ := json.Marshal(pinned)
	if _, err := fetchWithData(t, pinnedBytes); err == nil {
		t.Error("CPA FetchRegistry must reject pinned plugin version")
	}

	// 2. Unsupported schema_version must be rejected by CPA client.
	var badSchema map[string]any
	if err := json.Unmarshal(data, &badSchema); err != nil {
		t.Fatal(err)
	}
	badSchema["schema_version"] = 999
	badSchemaBytes, _ := json.Marshal(badSchema)
	if _, err := fetchWithData(t, badSchemaBytes); err == nil {
		t.Error("CPA FetchRegistry must reject unsupported schema_version 999")
	}

	// 3. Missing plugin ID must fail CPA ValidateRegistry.
	var missingID map[string]any
	if err := json.Unmarshal(data, &missingID); err != nil {
		t.Fatal(err)
	}
	missingID["plugins"].([]any)[0].(map[string]any)["id"] = ""
	missingIDBytes, _ := json.Marshal(missingID)
	if _, err := fetchWithData(t, missingIDBytes); err == nil {
		t.Error("CPA FetchRegistry must reject plugin with empty id")
	}
}
