// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestOpenCodeTrustedPermissionScope(t *testing.T) {
	scope, err := newOpenCodeWriteScope("/fixture", []string{"app.go", "app_test.go"}, []string{"app.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.rules) != 4 || scope.rules[0].Effect != "deny" ||
		scope.rules[2] != (openCodeNativeRule{Action: "read", Resource: "app_test.go", Effect: "allow"}) ||
		scope.rules[3] != (openCodeNativeRule{Action: "edit", Resource: "app.go", Effect: "allow"}) {
		t.Fatalf("unexpected permissions: %+v", scope.rules)
	}
	for _, paths := range [][]string{{"*.go"}, {"app?.go"}, {"~/app.go"}, {"../app.go"}, {".git/index"}} {
		if _, err := newOpenCodeWriteScope("/fixture", paths, paths); err == nil {
			t.Fatalf("allowed ambiguous or unsafe paths: %v", paths)
		}
	}
	if _, err := newOpenCodeWriteScope("/fixture", []string{"app.go"}, []string{"app_test.go"}); err == nil {
		t.Fatal("allowed write outside read scope")
	}
}

func TestOpenCodeTrustedNativeWriterEvidence(t *testing.T) {
	const transcript = `[
		{"type":"idle","outcome":"succeeded","time":{"created":8}},
		{"type":"assistant","agent":"build","model":{"id":"glm-5.3-flash-free","providerID":"opencode"},"content":[{"type":"text","text":"Updated the boundary."}],"finish":"stop","cost":0,"tokens":{"input":900,"output":12},"time":{"created":6,"completed":7}},
		{"type":"assistant","agent":"build","model":{"id":"glm-5.3-flash-free","providerID":"opencode"},"content":[{"type":"tool","id":"call_2","name":"edit","state":{"status":"completed","input":{"path":"app.go","oldString":"old","newString":"new"}}}],"finish":"tool-calls","cost":0,"time":{"created":4,"completed":5}},
		{"type":"assistant","agent":"build","model":{"id":"glm-5.3-flash-free","providerID":"opencode"},"content":[{"type":"tool","id":"call_1","name":"read","state":{"status":"completed","input":{"path":"/fixture/app.go"}}}],"finish":"tool-calls","cost":0,"time":{"created":2,"completed":3}},
		{"type":"user","id":"msg_fixture","text":"Fix the boundary.","time":{"created":1}}]`
	for _, tc := range []struct {
		name, before, after, code string
	}{
		{name: "verified native read and edit"},
		{name: "reject protected test write", before: `"path":"app.go"`, after: `"path":"app_test.go"`, code: "PERMISSION_DENIED"},
		{name: "reject external read", before: "/fixture/app.go", after: "/fixture-other/app.go", code: "PERMISSION_DENIED"},
		{name: "reject shell tool", before: `"name":"edit"`, after: `"name":"shell"`, code: "PERMISSION_DENIED"},
		{name: "reject denied edit", before: `"name":"edit","state":{"status":"completed"`, after: `"name":"edit","state":{"status":"error"`, code: "PERMISSION_DENIED"},
		{name: "reject no edit", before: `"name":"edit"`, after: `"name":"read"`, code: "INVALID_OUTPUT"},
		{name: "reject paid tool turn", before: `"cost":0,"time":{"created":4`, after: `"cost":1,"time":{"created":4`, code: "ZERO_COST_UNVERIFIED"},
		{name: "retain upstream cooldown", before: `"finish":"stop"`, after: `"finish":"stop","retry":{"error":{"status":429,"retryAfterMs":30000}}`, code: "RATE_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := transcript
			if tc.before != "" {
				input = strings.Replace(input, tc.before, tc.after, 1)
			}
			var messages []openCodeNativeMessage
			if err := json.Unmarshal([]byte(input), &messages); err != nil {
				t.Fatal(err)
			}
			scope, err := newOpenCodeWriteScope("/fixture", []string{"app.go", "app_test.go"}, []string{"app.go"})
			if err != nil {
				t.Fatal(err)
			}
			var evidence model.OpenCodeRunEvidence
			got, err := nativeWriterCompletion(messages, "msg_fixture", "glm-5.3-flash-free", "Fix the boundary.", scope, &evidence)
			if tc.code != "" {
				var fault *model.Fault
				if !errors.As(err, &fault) || fault.Code != tc.code || got != "" || evidence.TerminalVerified {
					t.Fatalf("expected %s, got %q %v %+v", tc.code, got, err, evidence)
				}
				return
			}
			if err != nil || got != "Updated the boundary." || !evidence.TerminalVerified || evidence.Model != openCodeNativeModel ||
				evidence.ReportedCost == nil || *evidence.ReportedCost != 0 ||
				evidence.ObservedTools["read"] != 1 || evidence.ObservedTools["edit"] != 1 {
				t.Fatalf("unexpected native evidence: %q %+v %v", got, evidence, err)
			}
		})
	}
}
