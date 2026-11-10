// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

type openCodeNativeTestTransport func(*http.Request) (*http.Response, error)

func (f openCodeNativeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenCodeNativeTransportRequiresExactModel(t *testing.T) {
	if !openCodeTransportMatches(openCodeNativeModel, model.OpenCodeNativeReadAsk) ||
		openCodeTransportMatches("opencode/big-pickle", model.OpenCodeNativeReadAsk) ||
		!openCodeTransportMatches("opencode/big-pickle", "") || openCodeTransportMatches(openCodeNativeModel, "unknown") {
		t.Fatal("native and stateless OpenCode admission were not kept distinct")
	}
}

func TestOpenCodeNativeGuardAndPrompt(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failure          string
		savedPermissions bool
		wantPrompts      int
		wantRejects      int
		before           string
		after            string
	}{
		{name: "terminal text and observed usage", wantPrompts: 1},
		{name: "rejects permission and interrupts the owned session", failure: "PERMISSION_DENIED", wantPrompts: 1, wantRejects: 1},
		{name: "never prompts with saved permissions", failure: "POLICY_DENIED", savedPermissions: true},
		{name: "wrong model", failure: "POLICY_DENIED", wantPrompts: 1,
			before: `"id":"glm-5.3-flash-free"`, after: `"id":"deepseek-v4.1-flash-free"`},
		{name: "tool content takes precedence over upstream retry", failure: "PERMISSION_DENIED", wantPrompts: 1,
			before: `"content":[{"type":"reasoning"`, after: `"retry":{"error":{"status":429,"retryAfterMs":30000}},"content":[{"type":"tool"`},
		{name: "upstream retry interrupts the owned session", failure: "RATE_LIMIT", wantPrompts: 1,
			before: `"finish":"stop",`, after: `"finish":"stop","retry":{"error":{"status":429,"retryAfterMs":30000}},`},
		{name: "missing reported cost", failure: "ZERO_COST_UNVERIFIED", wantPrompts: 1,
			before: `"cost":0,`},
		{name: "missing reported usage", failure: "INVALID_OUTPUT", wantPrompts: 1,
			before: `"tokens":{"input":755,"output":17,"reasoning":69},`},
		{name: "unsuccessful idle outcome", failure: "INVALID_OUTPUT", wantPrompts: 1,
			before: `"outcome":"succeeded"`, after: `"outcome":"failed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "glm-5.3-flash-free"
			const dir = "/private/tmp/ahenk-native-fixture"
			const sid = "ses_0123456789abcdefghij"
			const requestID = "per_0123456789abcdefghij"
			const path = "/api/session/" + sid
			const prompt = "Synthetic marker request"
			var messageID string
			var prompts, rejects, interrupts int
			runtime := openCodeRuntime{url: "http://127.0.0.1:54321", workdir: dir, http: &http.Client{Transport: openCodeNativeTestTransport(func(r *http.Request) (*http.Response, error) {
				body, status := "", http.StatusOK
				switch r.Method + " " + r.URL.RequestURI() {
				case "GET /api/agent":
					body = `{"location":{"directory":"` + dir + `"},"data":[{"id":"build","steps":2}]}`
				case "GET /api/agent/title", "GET /api/agent/summary":
					body, status = `{"_tag":"AgentNotFoundError"}`, http.StatusNotFound
				case "GET /api/permission/saved":
					body = `{"data":[]}`
					if tc.savedPermissions {
						body = `{"data":[{}]}`
					}
				case "GET /api/skill", "GET /api/mcp":
					body = `{"data":[]}`
				case "GET /api/integration/opencode":
					body = `{"data":{"id":"opencode","connections":[]}}`
				case "POST /api/session":
					body = `{"data":{"id":"` + sid + `","title":"Ahenk synthetic task","agent":"build","model":{"id":"` + id + `","providerID":"opencode"},"location":{"directory":"` + dir + `"},"permissions":[{"action":"*","resource":"*","effect":"deny"},{"action":"read","resource":"*","effect":"ask"}]}}`
				case "POST " + path + "/prompt":
					prompts++
					var input struct {
						ID       string `json:"id"`
						Text     string `json:"text"`
						Delivery string `json:"delivery"`
						Metadata struct {
							Agent string            `json:"agent"`
							Model map[string]string `json:"model"`
						} `json:"metadata"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.HasPrefix(input.ID, "msg_") || input.Text != prompt ||
						input.Delivery != "steer" || input.Metadata.Agent != "build" || input.Metadata.Model["modelID"] != id || input.Metadata.Model["providerID"] != "opencode" {
						t.Fatalf("unexpected native admission: %+v, %v", input, err)
					}
					messageID = input.ID
					body = `{}`
				case "GET " + path + "/permission":
					body = `{"data":[]}`
					if tc.wantRejects != 0 && rejects == 0 {
						body = `{"data":[{"id":"` + requestID + `","sessionID":"` + sid + `","action":"read"}]}`
					}
				case "POST " + path + "/permission/" + requestID + "/reply":
					rejects++
					var reply map[string]string
					if err := json.NewDecoder(r.Body).Decode(&reply); err != nil || len(reply) != 1 || reply["decision"] != "reject" {
						t.Fatalf("unexpected permission response: %v, %v", reply, err)
					}
					status = http.StatusNoContent
				case "POST " + path + "/interrupt?resume=false":
					interrupts++
					if r.Context().Err() != nil {
						t.Fatal("owned session interrupt used a cancelled context")
					}
					body = `{"interrupted":true}`
				case "GET " + path + "/message?limit=10":
					if tc.wantRejects != 0 {
						t.Fatal("waited for output despite pending permission")
					}
					body = `{"cursor":"ordinary-native-cursor","data":[{"type":"idle","outcome":"succeeded","time":{"created":4}},` +
						`{"type":"assistant","agent":"build","model":{"id":"` + id + `","providerID":"opencode"},` +
						`"content":[{"type":"reasoning","text":"not user output"},{"type":"text","text":"verified marker"}],` +
						`"finish":"stop","cost":0,"tokens":{"input":755,"output":17,"reasoning":69},"time":{"created":2,"completed":3}},` +
						`{"type":"user","id":"` + messageID + `","text":"` + prompt + `","time":{"created":1}}]}`
					if tc.before != "" {
						if !strings.Contains(body, tc.before) {
							t.Fatal("case does not modify the native message history")
						}
						body = strings.Replace(body, tc.before, tc.after, 1)
					}
				default:
					t.Fatalf("unexpected native request %s %s", r.Method, r.URL.RequestURI())
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			candidate := openCodeCatalogModel{ID: id, ModelID: id, Provider: "opencode", Type: "chat", Package: openCodeNativeSDK}
			candidate.Settings.BaseURL = openCodeNativeURL
			out, err := runtime.runNative(context.Background(), []openCodeCatalogModel{candidate}, id, prompt)
			wantInterrupts := 0
			if tc.wantPrompts == 1 && tc.failure != "" {
				wantInterrupts = 1
			}
			if prompts != tc.wantPrompts || rejects != tc.wantRejects || interrupts != wantInterrupts {
				t.Fatalf("unexpected control requests: prompts=%d rejects=%d interrupts=%d", prompts, rejects, interrupts)
			}
			if tc.failure != "" {
				var fault *model.Fault
				if !errors.As(err, &fault) || fault.Code != tc.failure || out.Summary != "" || out.OpenCode == nil || out.OpenCode.TerminalVerified {
					t.Fatalf("failure = %v, output = %+v", err, out)
				}
				return
			}
			e := out.OpenCode
			if err != nil || out.Summary != "verified marker" || e == nil || !e.GuardVerified || !e.PromptSent || !e.TerminalVerified ||
				e.NativeFinish != "stop" || e.Model != openCodeNativeModel || e.InputTokens != 755 || e.OutputTokens != 17 ||
				e.ReasoningTokens != 69 || e.ReportedCost == nil || *e.ReportedCost != 0 {
				t.Fatalf("unexpected verified result: %+v, %+v, %v", out, e, err)
			}
		})
	}
}

func TestOpenCodeNativeFaultUsesUpstreamStatusWithoutLocalWrapper(t *testing.T) {
	f, ok := (openCodeNativeFault{Status: 429, RetryAfter: 30_000}).error().(*model.Fault)
	if !ok || f.Code != "RATE_LIMIT" || f.UpstreamStatus != 429 || f.HTTPStatus != 0 || f.RetryAfterMS != 30_000 {
		t.Fatalf("unexpected native error projection: %+v", f)
	}
}

func TestOpenCodeChildInvocationRequiresProviderMetadata(t *testing.T) {
	const objective = "Compute 7 times 6"
	const parentPrompt = "Delegate the synthetic objective"
	const childID = "ses_0123456789abcdefghij"
	base := `[
		{"type":"user","id":"user","text":"Delegate the synthetic objective"},
		{"type":"assistant","agent":"build","model":{"providerID":"opencode","id":"glm-5.3-flash-free"},
		"time":{"completed":2},"cost":0,"content":[{"type":"tool","name":"subagent",
		"state":{"status":"completed","input":{"agent":"ahenk-native-child","description":"Ahenk synthetic child",
		"prompt":"Compute 7 times 6"},"metadata":{"sessionID":"ses_0123456789abcdefghij","status":"completed"}}}]},
		{"type":"idle","outcome":"succeeded"}
	]`
	for _, tc := range []struct{ name, before, after string }{
		{name: "linked foreground child"},
		{"text is insufficient", `"type":"tool","name":"subagent"`, `"type":"text","text":"42"`},
		{"foreign child input", `"agent":"ahenk-native-child"`, `"agent":"general"`},
		{"foreign model", `"id":"glm-5.3-flash-free"`, `"id":"paid-model"`},
		{"background launch is insufficient", `"status":"completed"}}`, `"status":"running"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			if tc.before != "" {
				input = strings.Replace(input, tc.before, tc.after, 1)
			}
			var messages []openCodeNativeMessage
			if err := json.Unmarshal([]byte(input), &messages); err != nil {
				t.Fatal(err)
			}
			id, done, err := openCodeChildInvocation(messages, "glm-5.3-flash-free", objective, "user", parentPrompt)
			if tc.before == "" {
				if err != nil || !done || id != childID {
					t.Fatalf("owned child not verified: %v %v %q", err, done, id)
				}
			} else if err == nil || done {
				t.Fatal("accepted unverified child")
			}
		})
	}
}

func TestOpenCodeBackgroundChildRequiresCorrelatedNotification(t *testing.T) {
	const child = "ses_0123456789abcdefghij"
	const objective = "Compute 7 times 6"
	const prompt = "Delegate the synthetic objective"
	const initial = `[
		{"type":"user","id":"user","text":"Delegate the synthetic objective"},
		{"type":"assistant","agent":"build","model":{"providerID":"opencode","id":"glm-5.3-flash-free"},
		"time":{"completed":2},"cost":0,"content":[{"type":"tool","name":"subagent",
		"state":{"status":"completed","input":{"agent":"ahenk-native-child","description":"Ahenk synthetic child",
		"prompt":"Compute 7 times 6","background":true},"metadata":{"sessionID":"ses_0123456789abcdefghij","status":"running"}}}]},
		{"type":"idle","outcome":"succeeded"}
	]`
	var messages []openCodeNativeMessage
	if err := json.Unmarshal([]byte(initial), &messages); err != nil {
		t.Fatal(err)
	}
	id, done, err := openCodeChildInvocation(messages, "glm-5.3-flash-free", objective, "user", prompt, true)
	if err != nil || done || id != child {
		t.Fatalf("launch mistaken for completion: %q %v %v", id, done, err)
	}
	n := openCodeNativeMessage{Type: "synthetic", Text: `<subagent sessionID="ses_0123456789abcdefghij" state="completed" description="Ahenk synthetic child">` + "\n42\n</subagent>",
		Metadata: []byte(`{"source":"subagent","childID":"ses_0123456789abcdefghij","agent":"ahenk-native-child","state":"completed"}`)}
	messages = append(messages, n)
	id, done, err = openCodeChildInvocation(messages, "glm-5.3-flash-free", objective, "user", prompt, true)
	if err != nil || !done || id != child {
		t.Fatalf("owned completion rejected: %q %v %v", id, done, err)
	}
	messages[len(messages)-1].Metadata = []byte(`{"source":"subagent","childID":"ses_abcdefghij0123456789","agent":"ahenk-native-child","state":"completed"}`)
	if _, _, err := openCodeChildInvocation(messages, "glm-5.3-flash-free", objective, "user", prompt, true); err == nil {
		t.Fatal("foreign completion accepted")
	}
}
