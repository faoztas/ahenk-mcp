// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaObject returns a nested object member of a decoded JSON schema.
func schemaObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := parent[key]
	if !ok {
		t.Fatalf("schema is missing %q: %#v", key, parent)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("schema member %q is not an object: %#v", key, raw)
	}
	return obj
}

// schemaAcceptedTypes reports the JSON types a schema property admits, following
// anyOf/oneOf/allOf branches and local $ref definitions.
func schemaAcceptedTypes(schema, root map[string]any, seen map[string]bool) map[string]bool {
	out := map[string]bool{}
	switch typ := schema["type"].(type) {
	case string:
		out[typ] = true
	case []any:
		for _, item := range typ {
			if s, ok := item.(string); ok {
				out[s] = true
			}
		}
	}
	if ref, ok := schema["$ref"].(string); ok {
		const prefix = "#/$defs/"
		if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
			name := ref[len(prefix):]
			if !seen[name] {
				seen[name] = true
				if defs, ok := root["$defs"].(map[string]any); ok {
					if def, ok := defs[name].(map[string]any); ok {
						for typ := range schemaAcceptedTypes(def, root, seen) {
							out[typ] = true
						}
					}
				}
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for _, branch := range branches {
			if obj, ok := branch.(map[string]any); ok {
				for typ := range schemaAcceptedTypes(obj, root, seen) {
					out[typ] = true
				}
			}
		}
	}
	return out
}

func TestServer_ToolsList_DelegateTaskDualFormSchemas(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error listing tools: %v", err)
	}
	toolMap := make(map[string]*mcp.Tool)
	for _, tool := range list.Tools {
		toolMap[tool.Name] = tool
	}

	for _, name := range []string{"delegate_task", "spawn_subagent"} {
		tool, ok := toolMap[name]
		if !ok {
			t.Fatalf("expected %s tool to be registered", name)
		}
		root, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s InputSchema is not an object: %#v", name, tool.InputSchema)
		}
		props := schemaObject(t, root, "properties")
		for _, field := range []string{"task", "target"} {
			prop := schemaObject(t, props, field)
			types := schemaAcceptedTypes(prop, root, map[string]bool{})
			if !types["string"] || !types["object"] {
				t.Fatalf("%s.%s must advertise both string and object forms, got %#v", name, field, prop)
			}
			if description, _ := prop["description"].(string); description == "" {
				t.Fatalf("%s.%s lost its discovery description: %#v", name, field, prop)
			}
		}
	}

	// Unrelated tool schemas must keep their existing shape.
	waitRoot := toolMap["wait_jobs"].InputSchema.(map[string]any)
	waitProps := schemaObject(t, waitRoot, "properties")
	jobIDs := schemaObject(t, waitProps, "job_ids")
	if types := schemaAcceptedTypes(jobIDs, waitRoot, map[string]bool{}); !types["array"] {
		t.Fatalf("wait_jobs.job_ids schema changed unexpectedly: %#v", jobIDs)
	}
	if _, wrapped := jobIDs["anyOf"]; wrapped {
		t.Fatalf("unrelated wait_jobs.job_ids must not be wrapped: %#v", jobIDs)
	}
}

func TestServer_DelegateTask_DualFormArgumentsAccepted(t *testing.T) {
	session, _ := setupTestMCPServer(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "string_task_string_target",
			args: map[string]any{
				"task":           "Inspect the build pipeline",
				"target":         "auto",
				"repo_id":        "test-repo",
				"execution_mode": "async",
			},
		},
		{
			name: "object_task_string_target",
			args: map[string]any{
				"task":           map[string]any{"category": "coding", "objective": "Inspect the build pipeline"},
				"target":         "auto",
				"repo_id":        "test-repo",
				"execution_mode": "async",
			},
		},
		{
			name: "string_task_object_target",
			args: map[string]any{
				"task":           "Inspect the build pipeline",
				"target":         map[string]any{"provider": "codex"},
				"repo_id":        "test-repo",
				"execution_mode": "async",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := callMCPTool(session, "delegate_task", tc.args)
			if err != nil {
				t.Fatalf("delegate_task call failed: %v", err)
			}
			if out["code"] != nil {
				t.Fatalf("expected accepted dual form, got error: %+v", out)
			}
			if id, _ := out["job_id"].(string); id == "" {
				t.Fatalf("expected job_id, got %+v", out)
			}
		})
	}
}
