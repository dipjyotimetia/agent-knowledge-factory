package schemas_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestExamplesMatchSchemas(t *testing.T) {
	root := repoRoot(t)
	cases := []struct {
		schema  string
		example string
	}{
		{"schemas/gap-report.schema.json", "schemas/examples/gap-report.json"},
		{"schemas/proposal.schema.json", "schemas/examples/proposal.json"},
	}
	for _, tc := range cases {
		t.Run(tc.example, func(t *testing.T) {
			compiler := jsonschema.NewCompiler()
			schemaPath := filepath.Join(root, tc.schema)
			if err := compiler.AddResource(schemaPath, mustDecode(t, schemaPath)); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(schemaPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(mustDecode(t, filepath.Join(root, tc.example))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustDecode(t *testing.T, path string) any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var value any
	if err := json.NewDecoder(f).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Dir(filepath.Dir(file))
}
