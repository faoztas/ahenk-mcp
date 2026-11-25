// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
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

func TestRoutingProfileDiscoveryAndCompatibility(t *testing.T) {
	session, _, engine := routingTestServer(t)
	list, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkEnum := func(props map[string]any) {
		t.Helper()
		field := schemaObject(t, props, "routing_profile")
		if !reflect.DeepEqual(field["enum"], []any{"price_performance", "quality", "price"}) {
			t.Fatalf("profile enum = %v", field)
		}
	}
	for _, tool := range list.Tools {
		if tool.Name != "delegate_task" && tool.Name != "spawn_subagent" && tool.Name != "delegate_batch" {
			continue
		}
		props := schemaObject(t, tool.InputSchema.(map[string]any), "properties")
		switch tool.Name {
		case "delegate_task", "spawn_subagent":
			checkEnum(props)
			task := schemaObject(t, props, "task")["anyOf"].([]any)[1].(map[string]any)
			checkEnum(schemaObject(t, task, "properties"))
		case "delegate_batch":
			checkEnum(props)
			item := schemaObject(t, schemaObject(t, props, "items"), "items")
			checkEnum(schemaObject(t, item, "properties"))
		}
	}
	for _, name := range []string{"delegate_task", "spawn_subagent"} {
		for _, tc := range []struct {
			task, target any
			top, want    string
		}{
			{"Inspect build", "auto", "price", "price"},
			{map[string]any{"objective": "Inspect build", "routing_profile": "quality"}, map[string]any{"provider": "codex"}, "", "quality"},
			{map[string]any{"objective": "Inspect build", "routing_profile": "quality"}, "auto", "quality", "quality"},
			{map[string]any{"objective": "Inspect build", "routing_profile": "quality"}, "auto", "price", ""},
			{"Inspect build", "auto", "other", ""},
		} {
			args := map[string]any{"repo_id": "test-repo", "task": tc.task, "target": tc.target, "execution_mode": "async"}
			if tc.top != "" {
				args["routing_profile"] = tc.top
			}
			out, err := callMCPTool(session, name, args)
			if err != nil {
				t.Fatal(err)
			}
			if out["schema_version"] != float64(1) {
				t.Fatalf("unexpected response version: %v", out["schema_version"])
			}
			if tc.want == "" {
				if out["code"] != "INVALID_INPUT" {
					t.Fatalf("%s accepted invalid profile: %v", name, out)
				}
				continue
			}
			id, _ := out["job_id"].(string)
			j, err := engine.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if string(j.RoutingProfile) != tc.want || j.RoutingProfileSource != "request" || j.Task.RoutingProfile != j.RoutingProfile {
				t.Fatalf("job profile %v/%v (explicit %v)", j.RoutingProfile, j.RoutingProfileSource, j.Task.RoutingProfile)
			}
		}
	}
}

func TestRoutingProfileWireModeDefaults(t *testing.T) {
	for _, tc := range []struct{ args, mode string }{
		{`{"task":"Inspect build","routing_profile":"price"}`, "wait"},
		{`{"task":{"objective":"Inspect build","routing_profile":"quality"}}`, "async"},
		{`{"objective":"Inspect build","routing_profile":"price_performance"}`, "async"},
	} {
		var in model.DelegateTaskInput
		if err := json.Unmarshal([]byte(tc.args), &in); err != nil {
			t.Fatal(err)
		}
		_, mode, _, err := in.Normalize("", nil)
		if err != nil || mode != tc.mode {
			t.Fatalf("mode = %q, want %q: %v", mode, tc.mode, err)
		}
	}
}

func TestRoutingProfileWireValidation(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	const batchItem = `{"key":"inspect","objective":"Inspect build","allowed_paths":["."],"mode":"read_only","data_class":"synthetic","acceptance":[{"id":"scope","description":"Describe the requested scope"}]`
	forms := []struct {
		name string
		tool string
		args func(string) string
	}{
		{"delegate top", "delegate_task", func(fields string) string {
			return `"task":"Inspect build","execution_mode":"async",` + fields
		}},
		{"delegate task", "delegate_task", func(fields string) string {
			return `"task":{"objective":"Inspect build",` + fields + `}`
		}},
		{"spawn task", "spawn_subagent", func(fields string) string {
			return `"task":{"objective":"Inspect build",` + fields + `}`
		}},
		{"batch top", "delegate_batch", func(fields string) string {
			return `"items":[` + batchItem + `}],` + fields
		}},
		{"batch item", "delegate_batch", func(fields string) string {
			return `"items":[` + batchItem + `,` + fields + `}]`
		}},
	}
	cases := []struct {
		name, fields string
		valid        bool
	}{
		{"duplicate", `"routing_profile":"quality","routing_profile":"price"`, false},
		{"same value twice", `"routing_profile":"quality","routing_profile":"quality"`, false},
		{"case variant", `"Routing_Profile":"quality"`, false},
		{"case variant after canonical", `"routing_profile":"quality","ROUTING_PROFILE":"price"`, false},
		{"escaped duplicate", `"routing_profile":"quality","routing_\u0070rofile":"price"`, false},
		{"unknown value", `"routing_profile":"default"`, false},
		{"invalid type", `"routing_profile":42`, false},
		{"null value", `"routing_profile":null`, false},
		{"unrelated fields", `"routing_profile":"quality","extra":{"ROUTING_PROFILE":null},"extra":false,"ObJeCtIvE":"Inspect build"`, true},
	}
	for i, form := range forms {
		for j, tc := range cases {
			t.Run(form.name+"/"+tc.name, func(t *testing.T) {
				args := fmt.Sprintf(`{"repo_id":"test-repo","request_id":"profile-wire-%d-%d",%s}`, i, j, form.args(tc.fields))
				out, err := callMCPTool(session, form.tool, json.RawMessage(args))
				if err != nil {
					t.Fatal(err)
				}
				id, _ := out["job_id"].(string)
				if tc.valid {
					if id == "" || out["code"] != nil {
						t.Fatalf("expected accepted payload, got code %v and job ID %q", out["code"], id)
					}
				} else if out["code"] != "INVALID_INPUT" || id != "" {
					t.Fatalf("expected INVALID_INPUT without admission, got code %v and job ID %q", out["code"], id)
				}
			})
		}
	}
}

func TestRoutingProfileConcurrentMCPClients(t *testing.T) {
	first, _, engine := routingTestServer(t)
	second := connectRoutingServer(t, engine, model.Config{}, providers.Registry{})
	for i, profile := range []string{"quality", "price"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			session := []*mcp.ClientSession{first, second}[i]
			out, err := callMCPTool(session, "delegate_task", map[string]any{"repo_id": "test-repo", "objective": "Inspect build", "routing_profile": profile})
			if err != nil {
				t.Fatal(err)
			}
			id, _ := out["job_id"].(string)
			j, err := engine.Get(id)
			if err != nil || string(j.RoutingProfile) != profile {
				t.Fatalf("job profile %v, want %s: %v", j.RoutingProfile, profile, err)
			}
		})
	}
}

func TestRoutingProfileBatchWire(t *testing.T) {
	session, _, engine := routingTestServer(t)
	item := func(key string) map[string]any {
		return map[string]any{"key": key, "objective": "Inspect code", "allowed_paths": []string{"."}, "mode": "read_only", "data_class": "synthetic",
			"acceptance": []model.Check{{ID: "scope", Description: "Describe the requested scope"}}}
	}
	one, two := item("one"), item("two")
	two["routing_profile"] = "quality"
	args := map[string]any{"repo_id": "test-repo", "request_id": "profile-batch", "routing_profile": "price", "items": []any{one, two}}
	out, err := callMCPTool(session, "delegate_batch", args)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := out["job_id"].(string)
	parent, err := engine.Get(id)
	if err != nil || len(parent.Children) != 2 {
		t.Fatalf("batch admission = %v: %v", out, err)
	}
	for i, profile := range []model.RoutingProfile{model.RoutingProfilePrice, model.RoutingProfileQuality} {
		child, err := engine.Get(parent.Children[i])
		if err != nil || child.RoutingProfile != profile {
			t.Fatalf("item %d profile %q: %v", i, child.RoutingProfile, err)
		}
	}
	args["routing_profile"] = "default"
	out, err = callMCPTool(session, "delegate_batch", args)
	if err != nil || out["code"] != "INVALID_INPUT" {
		t.Fatalf("invalid batch profile = %v: %v", out, err)
	}

	raw := json.RawMessage(`{"repo_id":"test-repo","request_id":"repeated-items-profile",
		"items":[{"key":"one","objective":"Inspect code","mode":"read_only","ROUTING_PROFILE":"quality"}],
		"items":[{"key":"one","objective":"Inspect code","mode":"read_only","data_class":"synthetic",
			"allowed_paths":["."],"acceptance":[{"id":"scope","description":"Describe scope"}]}]}`)
	out, err = callMCPTool(session, "delegate_batch", raw)
	if err != nil || out["code"] != "INVALID_INPUT" || out["job_id"] != nil {
		t.Fatalf("repeated items bypassed profile key validation: %v %v", out, err)
	}
}
