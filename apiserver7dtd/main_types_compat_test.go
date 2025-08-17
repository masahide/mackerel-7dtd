// main_types_compat_test.go
package main

import (
	"encoding/json"
	"testing"

	oapi "github.com/masahide/mackerel-7dtd/apiserver7dtd/internal/oapi"
)

func Test_OperationResult_JSONShape_Compatible(t *testing.T) {
	orig := OperationResult{
		Status: "started",
		Exec:   ExecResult{Command: "docker compose up -d"},
	}
	b, _ := json.Marshal(orig)

	var gen oapi.OperationResult
	if err := json.Unmarshal(b, &gen); err != nil {
		t.Fatalf("unmarshal into generated type: %v\njson=%s", err, string(b))
	}
	if gen.Status != "started" {
		t.Fatalf("status mismatch: %q", gen.Status)
	}
}
