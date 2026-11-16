package httpapi

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPICoversEveryRoute keeps api/openapi.yaml honest: every registered route
// must be documented, and every documented operation must exist. The frontend
// generates its client from that file, so drift here would break the dashboard.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Parameters  []struct {
				Ref string `yaml:"$ref"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	pathParam := regexp.MustCompile(`\{(\w+)\}`)
	documented := map[string]bool{}
	operationIDs := map[string]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			route := strings.ToUpper(method) + " " + pathParam.ReplaceAllString(path, ":$1")
			documented[route] = true
			if prev, dup := operationIDs[op.OperationID]; dup || op.OperationID == "" {
				t.Errorf("%s: operationId %q is empty or duplicates %s", route, op.OperationID, prev)
			}
			operationIDs[op.OperationID] = route
		}
	}

	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	registered := map[string]bool{}
	for _, r := range s.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	var missing, stale []string
	for route := range registered {
		if !documented[route] {
			missing = append(missing, route)
		}
	}
	for route := range documented {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("routes missing from api/openapi.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("api/openapi.yaml documents routes that do not exist:\n  %s", strings.Join(stale, "\n  "))
	}
}
