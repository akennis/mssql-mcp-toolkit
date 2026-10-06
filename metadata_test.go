package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLoadConfigMetadataFlags(t *testing.T) {
	// Create temporary files to satisfy the file presence validation in loadConfig
	tempDir := t.TempDir()
	file1 := filepath.Join(tempDir, "dbo.orders.md")
	if err := os.WriteFile(file1, []byte("orders doc"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	file2 := filepath.Join(tempDir, "dbo.customers.md")
	if err := os.WriteFile(file2, []byte("customers doc"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	subDir := filepath.Join(tempDir, "docs")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	cases := []struct {
		name    string
		args    []string
		wantErr string
		verify  func(*testing.T, *config)
	}{
		{
			name: "valid metadata files and directory",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-file", "dbo.orders:" + file1,
				"--metadata-file", "dbo.customers=" + file2,
				"--metadata-dir", subDir,
			},
			verify: func(t *testing.T, cfg *config) {
				wantFiles := map[string]string{
					"dbo.orders":    file1,
					"dbo.customers": file2,
				}
				if !reflect.DeepEqual(cfg.metadataFiles, wantFiles) {
					t.Errorf("metadataFiles = %v, want %v", cfg.metadataFiles, wantFiles)
				}
				if cfg.metadataDir != subDir {
					t.Errorf("metadataDir = %q, want %q", cfg.metadataDir, subDir)
				}
			},
		},
		{
			name: "invalid metadata file format",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-file", "invalid_format",
			},
			wantErr: "invalid metadata mapping",
		},
		{
			name: "missing metadata file",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-file", "dbo.missing:nonexistent_file.md",
			},
			wantErr: "metadata file for dbo.missing error",
		},
		{
			name: "missing metadata directory",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-dir", filepath.Join(tempDir, "nonexistent_dir"),
			},
			wantErr: "--metadata-dir path error",
		},
		{
			name: "metadata-dir schema",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-dir", subDir,
				"--metadata-dir-schema", " dbo ",
			},
			verify: func(t *testing.T, cfg *config) {
				if cfg.metadataDirSchema != "dbo" {
					t.Errorf("metadataDirSchema = %q, want %q", cfg.metadataDirSchema, "dbo")
				}
			},
		},
		{
			name: "metadata-dir schema without a directory",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-dir-schema", "dbo",
			},
			wantErr: "--metadata-dir-schema needs a --metadata-dir",
		},
		{
			name: "metadata-dir schema is qualified",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-dir", subDir,
				"--metadata-dir-schema", "db.dbo",
			},
			wantErr: "--metadata-dir-schema must be a bare schema name",
		},
		{
			name: "metadata-dir is a file",
			args: []string{
				"--conn-string", "server=localhost",
				"--tool-prefix", "sales",
				"--metadata-dir", file1,
			},
			wantErr: "--metadata-dir is not a directory",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("loadConfig succeeded, want error matching %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig failed: %v", err)
			}
			if tc.verify != nil {
				tc.verify(t, cfg)
			}
		})
	}
}

func TestMetadataTools(t *testing.T) {
	tempDir := t.TempDir()

	// Create specifically configured files
	file1 := filepath.Join(tempDir, "orders.md")
	if err := os.WriteFile(file1, []byte("# Orders Table\nDocumentation for orders."), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create directory for scraping
	docDir := filepath.Join(tempDir, "docs")
	if err := os.Mkdir(docDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	file2 := filepath.Join(docDir, "dbo.Customers.md")
	if err := os.WriteFile(file2, []byte("# Customers Table\nDocumentation for customers."), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	file3 := filepath.Join(docDir, "dbo.views.html")
	if err := os.WriteFile(file3, []byte("<h1>Views View</h1><p>Documentation for views.</p>"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Non-matching file in metadata directory
	if err := os.WriteFile(filepath.Join(docDir, "readme.txt"), []byte("readme"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &config{
		toolPrefix:   "sales",
		queryTimeout: 5 * time.Second,
		maxOpenConns: 1,
		metadataFiles: map[string]string{
			"dbo.orders": file1,
		},
		metadataDir: docDir,
	}

	ctx := context.Background()
	cs := connectTestClient(t, cfg)

	// Test list_metadata
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_list_metadata",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool sales_list_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("sales_list_metadata returned error: %s", contentText(res))
	}

	rawResult, err := jsonMarshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("jsonMarshal: %v", err)
	}
	var listRes listMetadataResult
	if err := jsonUnmarshal(rawResult, &listRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wantItems := []string{"dbo.customers", "dbo.orders", "dbo.views"}
	if !reflect.DeepEqual(listRes.Items, wantItems) {
		t.Errorf("listRes.Items = %v, want %v", listRes.Items, wantItems)
	}

	// Test get_metadata (specifically mapped)
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_get_metadata",
		Arguments: map[string]any{"name": "dbo.orders"},
	})
	if err != nil {
		t.Fatalf("CallTool sales_get_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("sales_get_metadata returned error: %s", contentText(res))
	}
	var getRes getMetadataResult
	rawResult, err = jsonMarshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("jsonMarshal: %v", err)
	}
	if err := jsonUnmarshal(rawResult, &getRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if getRes.Name != "dbo.orders" {
		t.Errorf("getRes.Name = %q, want dbo.orders", getRes.Name)
	}
	if !strings.Contains(getRes.Content, "Orders Table") {
		t.Errorf("getRes.Content = %q, want doc content", getRes.Content)
	}

	// Test get_metadata (scraped from directory case-insensitively)
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_get_metadata",
		Arguments: map[string]any{"name": "DBO.CUSTOMERS"},
	})
	if err != nil {
		t.Fatalf("CallTool sales_get_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("sales_get_metadata returned error: %s", contentText(res))
	}
	rawResult, err = jsonMarshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("jsonMarshal: %v", err)
	}
	if err := jsonUnmarshal(rawResult, &getRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if getRes.Name != "DBO.CUSTOMERS" {
		t.Errorf("getRes.Name = %q, want DBO.CUSTOMERS", getRes.Name)
	}
	if !strings.Contains(getRes.Content, "Customers Table") {
		t.Errorf("getRes.Content = %q, want doc content", getRes.Content)
	}

	// Test get_metadata (HTML file)
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_get_metadata",
		Arguments: map[string]any{"name": "dbo.views"},
	})
	if err != nil {
		t.Fatalf("CallTool sales_get_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("sales_get_metadata returned error: %s", contentText(res))
	}
	rawResult, err = jsonMarshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("jsonMarshal: %v", err)
	}
	if err := jsonUnmarshal(rawResult, &getRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if getRes.Name != "dbo.views" {
		t.Errorf("getRes.Name = %q, want dbo.views", getRes.Name)
	}
	if !strings.Contains(getRes.Content, "<h1>Views View</h1>") {
		t.Errorf("getRes.Content = %q, want HTML content", getRes.Content)
	}

	// Test get_metadata (nonexistent table/view)
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_get_metadata",
		Arguments: map[string]any{"name": "dbo.nonexistent"},
	})
	if err != nil {
		t.Fatalf("CallTool sales_get_metadata: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error for nonexistent metadata, but succeeded")
	}
}

// TestMetadataDirSchema covers a directory of documentation named for tables
// alone, with the schema they belong to supplied by configuration instead of by
// every file name.
func TestMetadataDirSchema(t *testing.T) {
	docDir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(docDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	write("Orders.md", "# Orders Table")
	write("Shipments.html", "<h1>Shipments View</h1>")
	// A file that names its own schema keeps it, even alongside a dir schema.
	write("ref.Regions.md", "# Regions Table")
	write("readme.txt", "readme")

	cfg := &config{
		toolPrefix:        "sales",
		queryTimeout:      5 * time.Second,
		maxOpenConns:      1,
		metadataDir:       docDir,
		metadataDirSchema: "dbo",
	}

	ctx := context.Background()
	cs := connectTestClient(t, cfg)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_list_metadata",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool sales_list_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("sales_list_metadata returned error: %s", contentText(res))
	}
	rawResult, err := jsonMarshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("jsonMarshal: %v", err)
	}
	var listRes listMetadataResult
	if err := jsonUnmarshal(rawResult, &listRes); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantItems := []string{"dbo.orders", "dbo.shipments", "ref.regions"}
	if !reflect.DeepEqual(listRes.Items, wantItems) {
		t.Errorf("listRes.Items = %v, want %v", listRes.Items, wantItems)
	}

	getCases := []struct {
		name        string
		wantContent string
	}{
		{"dbo.orders", "# Orders Table"},
		{"DBO.Orders", "# Orders Table"},
		{"dbo.shipments", "<h1>Shipments View</h1>"},
		{"ref.regions", "# Regions Table"},
	}
	for _, tc := range getCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      "sales_get_metadata",
				Arguments: map[string]any{"name": tc.name},
			})
			if err != nil {
				t.Fatalf("CallTool sales_get_metadata: %v", err)
			}
			if res.IsError {
				t.Fatalf("sales_get_metadata returned error: %s", contentText(res))
			}
			rawResult, err := jsonMarshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("jsonMarshal: %v", err)
			}
			var getRes getMetadataResult
			if err := jsonUnmarshal(rawResult, &getRes); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !strings.Contains(getRes.Content, tc.wantContent) {
				t.Errorf("getRes.Content = %q, want it to contain %q", getRes.Content, tc.wantContent)
			}
		})
	}

	// The dir schema qualifies the files it covers; it does not make every
	// schema resolve to them.
	for _, name := range []string{"orders", "other.orders", "dbo.regions"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "sales_get_metadata",
			Arguments: map[string]any{"name": name},
		})
		if err != nil {
			t.Fatalf("CallTool sales_get_metadata %s: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("sales_get_metadata(%q) succeeded, want an error", name)
		}
	}
}

// TestMetadataDirWithoutSchema pins the behaviour a dir schema changes: with no
// schema configured, an unqualified file name has nothing to qualify it and is
// left out.
func TestMetadataDirWithoutSchema(t *testing.T) {
	docDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(docDir, "Orders.md"), []byte("# Orders Table"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := &metadataManager{dir: docDir}
	keys, err := m.getMetadataKeys()
	if err != nil {
		t.Fatalf("getMetadataKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("getMetadataKeys() = %v, want none", keys)
	}
	if _, err := m.getMetadataContent("orders"); err == nil {
		t.Error("getMetadataContent(\"orders\") succeeded, want an error")
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
