package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stringMapFlag implements flag.Value to support repeating key:value flags.
type stringMapFlag map[string]string

func (f stringMapFlag) String() string {
	if len(f) == 0 {
		return ""
	}
	var pairs []string
	for k, v := range f {
		pairs = append(pairs, fmt.Sprintf("%s:%s", k, v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ", ")
}

func (f stringMapFlag) Set(value string) error {
	idxColon := strings.Index(value, ":")
	idxEq := strings.Index(value, "=")

	var sepIdx int
	if idxColon >= 0 && idxEq >= 0 {
		if idxColon < idxEq {
			sepIdx = idxColon
		} else {
			sepIdx = idxEq
		}
	} else if idxColon >= 0 {
		sepIdx = idxColon
	} else if idxEq >= 0 {
		sepIdx = idxEq
	} else {
		return fmt.Errorf("invalid metadata mapping %q, expected format is schema.table:path/to/file.md or schema.table:path/to/file.html", value)
	}

	k := value[:sepIdx]
	v := value[sepIdx+1:]
	if k == "" || v == "" {
		return fmt.Errorf("invalid metadata mapping %q, expected format is schema.table:path/to/file.md or schema.table:path/to/file.html", value)
	}
	f[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	return nil
}

type metadataManager struct {
	fileMap map[string]string
	dir     string
	// dirSchema is the schema that files in dir document when their names do
	// not carry one. It is what lets a directory of Orders.md / Customers.md
	// answer for dbo.orders and dbo.customers.
	dirSchema string
}

// metadataExtensions are the documentation file types served out of dir, in
// the order they are tried when a name could be answered by more than one.
var metadataExtensions = []string{".md", ".html"}

// trimMetadataExt strips a documentation extension from a file name, reporting
// whether the name had one at all.
func trimMetadataExt(name string) (string, bool) {
	for _, ext := range metadataExtensions {
		if len(name) > len(ext) && strings.EqualFold(name[len(name)-len(ext):], ext) {
			return name[:len(name)-len(ext)], true
		}
	}
	return "", false
}

// schemaPrefix is dirSchema in the lowercase, dot-terminated form that keys
// are compared against. It is empty when no schema is configured.
func (m *metadataManager) schemaPrefix() string {
	schema := strings.ToLower(strings.TrimSpace(m.dirSchema))
	if schema == "" {
		return ""
	}
	return schema + "."
}

// keyForFile maps a file in dir to the qualified name it documents. A file
// that is already qualified (dbo.orders.md) keeps the schema it names; an
// unqualified one (orders.md) takes dirSchema, and is skipped entirely when no
// schema is configured, because there is then nothing to qualify it with.
func (m *metadataManager) keyForFile(name string) (string, bool) {
	base, ok := trimMetadataExt(name)
	if !ok {
		return "", false
	}
	switch strings.Count(base, ".") {
	case 0:
		prefix := m.schemaPrefix()
		if prefix == "" {
			return "", false
		}
		return prefix + strings.ToLower(base), true
	case 1:
		return strings.ToLower(base), true
	default:
		return "", false
	}
}

// baseNamesFor returns the file names, without extension, that could hold the
// documentation for key, most specific first. These are exactly the names that
// keyForFile maps back to key, so a name is served only if the listing offers
// it: a qualified file, and — when the key is in dirSchema — a file named for
// the table alone.
func (m *metadataManager) baseNamesFor(key string) []string {
	if strings.Count(key, ".") != 1 {
		return nil
	}
	bases := []string{key}
	if prefix := m.schemaPrefix(); prefix != "" {
		if table, ok := strings.CutPrefix(key, prefix); ok && table != "" {
			bases = append(bases, table)
		}
	}
	return bases
}

func (m *metadataManager) getMetadataKeys() ([]string, error) {
	keysMap := make(map[string]bool)
	for k := range m.fileMap {
		keysMap[k] = true
	}
	if m.dir != "" {
		files, err := os.ReadDir(m.dir)
		if err != nil {
			if os.IsNotExist(err) {
				return m.sortedKeys(keysMap), nil
			}
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if key, ok := m.keyForFile(f.Name()); ok {
				keysMap[key] = true
			}
		}
	}
	return m.sortedKeys(keysMap), nil
}

func (m *metadataManager) sortedKeys(keysMap map[string]bool) []string {
	keys := make([]string, 0, len(keysMap))
	for k := range keysMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *metadataManager) getMetadataContent(key string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if path, ok := m.fileMap[key]; ok {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading metadata file %s: %w", path, err)
		}
		return string(content), nil
	}

	if m.dir != "" {
		files, err := os.ReadDir(m.dir)
		if err != nil {
			return "", fmt.Errorf("reading metadata directory: %w", err)
		}
		for _, base := range m.baseNamesFor(key) {
			for _, ext := range metadataExtensions {
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					if strings.EqualFold(f.Name(), base+ext) {
						path := filepath.Join(m.dir, f.Name())
						content, err := os.ReadFile(path)
						if err != nil {
							return "", fmt.Errorf("reading metadata file %s: %w", path, err)
						}
						return string(content), nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no metadata associated with %s", key)
}

type getMetadataInput struct {
	Name string `json:"name" jsonschema:"the fully qualified table or view name, e.g. schema.table or schema.view"`
}

type getMetadataResult struct {
	Name    string `json:"name" jsonschema:"the qualified table or view name"`
	Content string `json:"content" jsonschema:"the markdown or html metadata content"`
}

type listMetadataInput struct {
}

type listMetadataResult struct {
	Items []string `json:"items" jsonschema:"the list of fully qualified table/view names that have metadata"`
}

func registerMetadataTools(server *mcp.Server, cfg *config) {
	database := displayDatabase(cfg)

	getMetadataName := toolName(cfg.toolPrefix, getMetadataToolSuffix)
	getMetadataDescription := cfg.getMetadataDescription
	if isBlank(getMetadataDescription) {
		getMetadataDescription = defaultGetMetadataToolDescription(database)
	}
	getMetadataInputSchema, err := jsonschema.For[getMetadataInput](nil)
	if err != nil {
		panic(fmt.Sprintf("building %s input schema: %v", getMetadataName, err))
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        getMetadataName,
		Title:       "Get Table or View Metadata",
		Description: getMetadataDescription,
		InputSchema: getMetadataInputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getMetadataInput) (*mcp.CallToolResult, *getMetadataResult, error) {
		manager := &metadataManager{fileMap: cfg.metadataFiles, dir: cfg.metadataDir, dirSchema: cfg.metadataDirSchema}
		content, err := manager.getMetadataContent(in.Name)
		if err != nil {
			return nil, nil, err
		}
		res := &getMetadataResult{
			Name:    in.Name,
			Content: content,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: content}},
		}, res, nil
	})

	listMetadataName := toolName(cfg.toolPrefix, listMetadataToolSuffix)
	listMetadataDescription := cfg.listMetadataDescription
	if isBlank(listMetadataDescription) {
		listMetadataDescription = defaultListMetadataToolDescription(database, cfg.toolPrefix)
	}
	listMetadataInputSchema, err := jsonschema.For[listMetadataInput](nil)
	if err != nil {
		panic(fmt.Sprintf("building %s input schema: %v", listMetadataName, err))
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        listMetadataName,
		Title:       "List Table/View Metadata",
		Description: listMetadataDescription,
		InputSchema: listMetadataInputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listMetadataInput) (*mcp.CallToolResult, *listMetadataResult, error) {
		manager := &metadataManager{fileMap: cfg.metadataFiles, dir: cfg.metadataDir, dirSchema: cfg.metadataDirSchema}
		items, err := manager.getMetadataKeys()
		if err != nil {
			return nil, nil, err
		}
		res := &listMetadataResult{
			Items: items,
		}
		var text string
		if len(items) == 0 {
			text = "No metadata files configured."
		} else {
			text = "Available metadata files:\n- " + strings.Join(items, "\n- ")
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, res, nil
	})
}
