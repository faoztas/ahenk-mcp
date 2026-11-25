// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"

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
		{"type":"user","id":"user","text":"Delegate the synthetic objective","time":{"created":1}},
		{"type":"assistant","agent":"build","model":{"providerID":"opencode","id":"glm-5.3-flash-free"},
		"time":{"created":2,"completed":3},"cost":0,"content":[{"type":"tool","name":"subagent",
		"state":{"status":"completed","input":{"agent":"ahenk-native-child","description":"Ahenk synthetic child",
		"prompt":"Compute 7 times 6"},"metadata":{"sessionID":"ses_0123456789abcdefghij","status":"completed"}}}]},
		{"type":"idle","outcome":"succeeded","time":{"created":4}}
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

func backgroundChildMessages(t *testing.T) []openCodeNativeMessage {
	t.Helper()
	const initial = `[
		{"type":"user","time":{"created":1},"id":"user","text":"Delegate the synthetic objective"},
		{"type":"assistant","id":"launch","agent":"build","model":{"providerID":"opencode","id":"glm-5.3-flash-free"},
		"time":{"created":2,"completed":3},"cost":0,"content":[{"type":"tool","name":"subagent",
		"state":{"status":"completed","input":{"agent":"ahenk-native-child","description":"Ahenk synthetic child",
		"prompt":"Compute 7 times 6","background":true},"metadata":{"sessionID":"ses_0123456789abcdefghij","status":"running"}}}]},
		{"type":"idle","id":"launch-idle","time":{"created":4},"outcome":"succeeded"}
	]`
	var messages []openCodeNativeMessage
	if err := json.Unmarshal([]byte(initial), &messages); err != nil {
		t.Fatal(err)
	}
	n := openCodeNativeMessage{Type: "synthetic", Text: `<subagent sessionID="ses_0123456789abcdefghij" state="completed" description="Ahenk synthetic child">` + "\n42\n</subagent>",
		Metadata: []byte(`{"source":"subagent","childID":"ses_0123456789abcdefghij","agent":"ahenk-native-child","state":"completed"}`)}
	n.ID = "notice"
	n.Time.Created = 5
	ack := messages[1]
	ack.ID = "ack"
	ack.Time.Created = 6
	ack.Time.Completed = 7
	ack.Finish = "stop"
	ack.Content = []openCodeNativePart{{Type: "text", Text: "Acknowledged"}}
	idle := messages[2]
	idle.ID = "ack-idle"
	idle.Time.Created = 8
	return append(messages, n, ack, idle)
}

func TestOpenCodeBackgroundChildRequiresCorrelatedNotification(t *testing.T) {
	const child = "ses_0123456789abcdefghij"
	m := backgroundChildMessages(t)
	check := func(messages []openCodeNativeMessage) (string, bool, error) {
		return openCodeChildInvocation(messages, testOpenCodeChildModel, testOpenCodeChildObjective, "user", "Delegate the synthetic objective", true)
	}
	if id, done, err := check(m[:3]); err != nil || done || id != child {
		t.Fatalf("launch mistaken for completion: %q %v %v", id, done, err)
	}
	if id, done, err := check(m); err != nil || !done || id != child {
		t.Fatalf("owned completion rejected: %q %v %v", id, done, err)
	}
	m[3].Metadata = []byte(`{"source":"subagent","childID":"ses_abcdefghij0123456789","agent":"ahenk-native-child","state":"completed"}`)
	if _, _, err := check(m); err == nil {
		t.Fatal("foreign completion accepted")
	}
}

func TestOpenCodeChildRequiresCorrelatedPostNoticeCompletion(t *testing.T) {
	for _, name := range []string{"success", "notice only", "streaming", "stale idle", "duplicate idle", "extra assistant", "newer failed idle first", "newer cancelled idle first", "missing cost", "nonzero cost", "notice before launch", "missing notice time", "missing acknowledgement text", "idle references another assistant", "ack references another notice", "launch references another prompt"} {
		t.Run(name, func(t *testing.T) {
			m := backgroundChildMessages(t)
			switch name {
			case "notice only":
				m = m[:4]
			case "streaming":
				m[4].Time.Completed = 0
			case "stale idle":
				m[5].Time.Created = 4
			case "duplicate idle":
				m = append(m, m[5])
			case "extra assistant":
				extra := m[4]
				extra.ID = "other"
				m = append(m, extra)
			case "newer failed idle first", "newer cancelled idle first":
				latest := m[5]
				latest.ID = "latest"
				latest.Time.Created++
				latest.Outcome = "failed"
				if name == "newer cancelled idle first" {
					latest.Outcome = "cancelled"
				}
				m = append([]openCodeNativeMessage{latest}, m...)
			case "missing cost":
				m[4].Cost = nil
			case "nonzero cost":
				cost := .01
				m[4].Cost = &cost
			case "notice before launch":
				m[3].Time.Created = 1
			case "missing notice time":
				m[3].Time.Created = 0
			case "missing acknowledgement text":
				m[4].Content = nil
			case "idle references another assistant", "ack references another notice", "launch references another prompt":
				i := map[string]int{"idle references another assistant": 5, "ack references another notice": 4, "launch references another prompt": 1}[name]
				if err := json.Unmarshal([]byte(`{"parentID":"foreign","messageID":"foreign"}`), &m[i]); err != nil {
					t.Fatal(err)
				}
			}
			_, done, err := openCodeChildInvocation(m, testOpenCodeChildModel, testOpenCodeChildObjective, "user", "Delegate the synthetic objective", true)
			if name == "success" && (err != nil || !done) || name != "success" && done {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
}

func TestOpenCodeChildIdleOnlyForeignRateLimit(t *testing.T) {
	var messages []openCodeNativeMessage
	if err := json.Unmarshal([]byte(`[{"type":"user","id":"user","text":"objective"},{"type":"idle","messageID":"foreign","error":{"status":429,"rateLimitScope":"account"}}]`), &messages); err != nil {
		t.Fatal(err)
	}
	summary, err := nativeTextCompletion(messages, "user", testOpenCodeChildModel, "objective", openCodeChildAgent, &model.OpenCodeRunEvidence{})
	var fault *model.Fault
	if summary != "" || !errors.As(err, &fault) || fault.Code != "INVALID_OUTPUT" || fault.ProviderRateLimitScope != "" {
		t.Fatalf("foreign idle supplied child result or provider fault: %q %+v", summary, fault)
	}
}

func TestOpenCodeChildPreservesTyped429(t *testing.T) {
	f := openCodeChildTestFixture{background: true, historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000,"rateLimitScope":"account"}},"finish":"stop"`}
	out, err := f.run(t)
	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "RATE_LIMIT" || fault.UpstreamStatus != 429 || fault.ProviderRateLimitScope != "account" || out.OpenCode.TerminalVerified {
		t.Fatal(err, out.OpenCode)
	}
}

func TestOpenCodeNativeChildFaultRequiresOwnedTimeline(t *testing.T) {
	const faultJSON = `{"status":429,"retryAfterMs":30000,"rateLimitScope":"account"}`
	for _, tc := range []struct {
		name                     string
		prompt, start, end, idle int64
		faultOn                  string
		wantRetry                bool
	}{
		{"assistant retry predates prompt", 4, 2, 3, 0, "retry", false},
		{"assistant error completion predates start", 1, 3, 2, 0, "assistant", false},
		{"idle error predates completion", 1, 2, 4, 3, "idle", false},
		{"idle error predates unfinished assistant", 1, 3, 0, 2, "idle", false},
		{"retry without owned prompt", 0, 2, 0, 0, "retry", false},
		{"owned retry while assistant is running", 1, 2, 0, 0, "retry", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assistant := openCodeNativeMessage{
				Type: "assistant", Agent: openCodeChildAgent,
				Model: openCodeNativeRef{Provider: "opencode", ID: testOpenCodeChildModel},
			}
			assistant.Time.Created, assistant.Time.Completed = tc.start, tc.end
			idle := openCodeNativeMessage{Type: "idle"}
			idle.Time.Created = tc.idle
			upstream := openCodeNativeFault{Status: 429, RateScope: "account", RetryAfter: 30000}
			switch tc.faultOn {
			case "retry":
				if err := json.Unmarshal([]byte(`{"error":`+faultJSON+`}`), &assistant.Retry); err != nil {
					t.Fatal(err)
				}
			case "assistant":
				assistant.Error = &upstream
			case "idle":
				idle.Error = &upstream
			}
			messages := []openCodeNativeMessage{assistant}
			if tc.prompt != 0 {
				prompt := openCodeNativeMessage{Type: "user", ID: "user", Text: "objective"}
				prompt.Time.Created = tc.prompt
				messages = append(messages, prompt)
			}
			if tc.idle != 0 {
				messages = append(messages, idle)
			}
			_, err := nativeTextCompletion(messages, "user", testOpenCodeChildModel, "objective", openCodeChildAgent, &model.OpenCodeRunEvidence{})
			var fault *model.Fault
			if !errors.As(err, &fault) {
				t.Fatalf("expected typed fault, got %v", err)
			}
			if tc.wantRetry {
				if fault.Code != "RATE_LIMIT" || fault.ProviderRateLimitScope != "account" || fault.UpstreamStatus != 429 || fault.RetryAfterMS != 30000 {
					t.Fatalf("lost owned retry: %+v", fault)
				}
			} else if fault.Code != "INVALID_OUTPUT" || fault.ProviderRateLimitScope != "" || fault.UpstreamStatus != 0 || fault.RetryAfterMS != 0 {
				t.Fatalf("unowned event supplied a provider fault: %+v", fault)
			}
		})
	}
}

func TestOpenCodeNativeChildRejectsUnownedCompletionAndKeeps429(t *testing.T) {
	for _, tc := range []struct {
		name, code                  string
		background, noNotice        bool
		sessionBefore, sessionAfter string
		parentBefore, parentAfter   string
		historyBefore, historyAfter string
		wantChildInterrupt          bool
		childPermission             bool
	}{
		{name: "foreground idle predates launch completion", code: "INVALID_OUTPUT", parentBefore: `"created":4`, parentAfter: `"created":2`, wantChildInterrupt: true},
		{name: "foreground launch predates prompt", code: "INVALID_OUTPUT", parentBefore: `"created":1`, parentAfter: `"created":3`, wantChildInterrupt: true},
		{name: "foreground launch completion predates start", code: "INVALID_OUTPUT", parentBefore: `"completed":3`, parentAfter: `"completed":1`, wantChildInterrupt: true},
		{name: "foreground parent idle names foreign launch", code: "INVALID_OUTPUT", parentBefore: `"type":"idle"`, parentAfter: `"type":"idle","messageID":"other-turn"`, wantChildInterrupt: true},
		{name: "foreground launch names foreign prompt", code: "INVALID_OUTPUT", parentBefore: `"type":"assistant"`, parentAfter: `"type":"assistant","parentID":"other-turn"`},
		{name: "foreground latest idle failed", code: "INVALID_OUTPUT", parentBefore: `{"type":"idle","outcome":"succeeded","time":{"created":4}}`, parentAfter: `{"type":"idle","outcome":"failed","time":{"created":5}},{"type":"idle","outcome":"succeeded","time":{"created":4}}`, wantChildInterrupt: true},
		{name: "foreground child with wrong parent", code: "POLICY_DENIED", sessionBefore: "ses_parent0123456789", sessionAfter: "ses_otherparent123456"},
		{name: "background child with wrong permissions", background: true, code: "POLICY_DENIED", sessionBefore: `"effect":"deny"`, sessionAfter: `"effect":"allow"`},
		{name: "foreground child with wrong model", code: "POLICY_DENIED", sessionBefore: `"id":"glm-5.3-flash-free"`, sessionAfter: `"id":"paid-model"`},
		{name: "parent tool completion from unfinished assistant", code: "INVALID_OUTPUT", parentBefore: `"completed":3`, parentAfter: `"completed":0`, wantChildInterrupt: true},
		{name: "malformed tool metadata", code: "INVALID_OUTPUT", parentBefore: `"status":"completed"}}`, parentAfter: `"status":"completed","status":false}}`},
		{name: "pending child permission is rejected once", code: "PERMISSION_DENIED", childPermission: true, wantChildInterrupt: true},
		{name: "child answer belongs to another user", background: true, code: "INVALID_OUTPUT", historyBefore: `"type":"assistant"`, historyAfter: `"type":"assistant","parentID":"other-turn"`, wantChildInterrupt: true},
		{name: "child idle names another assistant", background: true, code: "INVALID_OUTPUT", historyBefore: `"type":"idle"`, historyAfter: `"type":"idle","messageID":"other-turn"`, wantChildInterrupt: true},
		{name: "child idle belongs to another assistant", background: true, code: "INVALID_OUTPUT", historyBefore: `"type":"idle"`, historyAfter: `"type":"idle","parentID":"other-turn"`, wantChildInterrupt: true},
		{name: "background notice without independent terminal", background: true, code: "INVALID_OUTPUT", historyBefore: `"completed":3`, historyAfter: `"completed":0`, wantChildInterrupt: true},
		{name: "background notice with different result", background: true, code: "INVALID_OUTPUT", historyBefore: `"text":"42"`, historyAfter: `"text":"99"`, wantChildInterrupt: true},
		{name: "foreground upstream retry", code: "RATE_LIMIT", historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000,"rateLimitScope":"model"}},"finish":"stop"`, wantChildInterrupt: true},
		{name: "foreground tool error preserves owned retry", code: "RATE_LIMIT", parentBefore: `"status":"completed"`, parentAfter: `"status":"error"`, historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000,"rateLimitScope":"model"}},"finish":"stop"`, wantChildInterrupt: true},
		{name: "background upstream retry without notice", background: true, noNotice: true, code: "RATE_LIMIT", historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000,"rateLimitScope":"model"}},"finish":"stop"`, wantChildInterrupt: true},
		{name: "background upstream retry with failure notice", background: true, code: "RATE_LIMIT", parentBefore: `"state":"completed"`, parentAfter: `"state":"failed"`, historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000,"rateLimitScope":"model"}},"finish":"stop"`, wantChildInterrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openCodeChildTestFixture{
				background: tc.background, noNotice: tc.noNotice, sessionBefore: tc.sessionBefore, sessionAfter: tc.sessionAfter,
				parentBefore: tc.parentBefore, parentAfter: tc.parentAfter,
				historyBefore: tc.historyBefore, historyAfter: tc.historyAfter,
				childPermission: tc.childPermission,
			}
			out, err := fixture.run(t)
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != tc.code {
				t.Errorf("want %s, got %v", tc.code, err)
			} else if tc.code == "RATE_LIMIT" &&
				(fault.UpstreamStatus != 429 || fault.HTTPStatus != 0 || fault.RetryAfterMS != 30000 || fault.RetrySource != "provider_body" || fault.ProviderRateLimitScope != "model") {
				t.Errorf("native retry scope or hints lost: %+v", fault)
			}
			if out.Summary != "" || out.OpenCode.TerminalVerified || out.OpenCode.NativeChild != nil || out.OpenCode.ReportedCost != nil {
				t.Errorf("failed run exposed success evidence: %+v", out.OpenCode)
			}
			if fixture.prompts != 1 || fixture.parentInterrupts != 1 || (fixture.childInterrupts == 1) != tc.wantChildInterrupt || fixture.childInterrupts > 1 {
				t.Errorf("unexpected requests: %d prompts, %d parent interrupts, %d child interrupts", fixture.prompts, fixture.parentInterrupts, fixture.childInterrupts)
			}
			if (fixture.childRejects == 1) != tc.childPermission || fixture.childRejects > 1 {
				t.Errorf("unexpected child permission rejections: %d", fixture.childRejects)
			}
		})
	}
	t.Run("cancel after launch before parent assistant finishes", func(t *testing.T) {
		fixture := openCodeChildTestFixture{
			background: true, noNotice: true, cancelOnChildHistory: true,
			parentBefore: `"time":{"created":2,"completed":3}`, parentAfter: `"time":{"created":2}`,
		}
		out, err := fixture.run(t)
		if !errors.Is(err, context.Canceled) || out.Summary != "" || out.OpenCode.NativeChild != nil {
			t.Errorf("want cancellation without acceptance evidence, got %v and %+v", err, out.OpenCode)
		}
		if fixture.prompts != 1 || fixture.parentInterrupts != 1 || fixture.childInterrupts != 1 {
			t.Errorf("unexpected requests: %d prompts, %d parent interrupts, %d child interrupts", fixture.prompts, fixture.parentInterrupts, fixture.childInterrupts)
		}
	})
	t.Run("verified permission survives an unavailable cleanup read", func(t *testing.T) {
		fixture := openCodeChildTestFixture{childPermission: true, failRepeatedPermissionRead: true}
		_, err := fixture.run(t)
		var fault *model.Fault
		if !errors.As(err, &fault) || fault.Code != "PERMISSION_DENIED" {
			t.Fatalf("want PERMISSION_DENIED, got %v", err)
		}
		if fixture.childRejects != 1 || fixture.childInterrupts != 1 {
			t.Fatalf("want one rejection and interrupt for the owned child, got %d and %d", fixture.childRejects, fixture.childInterrupts)
		}
	})
}

func TestOpenCodeNativeAcceptanceRequiresOwnedTerminalAndCost(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		fixture    openCodeChildTestFixture
	}{
		{name: "parent-only reply", code: "INVALID_OUTPUT", fixture: openCodeChildTestFixture{
			parentBefore: `"type":"tool","name":"subagent"`, parentAfter: `"type":"text","text":"42"`}},
		{name: "wrong child row identity", code: "POLICY_DENIED", fixture: openCodeChildTestFixture{
			background: true, sessionBefore: testOpenCodeChildID, sessionAfter: "ses_otherchild123456"}},
		{name: "ambiguous child cost", code: "ZERO_COST_UNVERIFIED", fixture: openCodeChildTestFixture{
			historyBefore: `"cost":0,`}},
		{name: "nonzero child cost", code: "ZERO_COST_UNVERIFIED", fixture: openCodeChildTestFixture{
			background: true, historyBefore: `"cost":0`, historyAfter: `"cost":0.01`}},
		{name: "ambiguous parent cost", code: "ZERO_COST_UNVERIFIED", fixture: openCodeChildTestFixture{
			parentBefore: `"cost":0,`}},
		{name: "cooldown without asserted scope", code: "RATE_LIMIT", fixture: openCodeChildTestFixture{
			background: true, noNotice: true, historyBefore: `"finish":"stop"`, historyAfter: `"retry":{"error":{"status":429,"retryAfterMs":30000}},"finish":"stop"`}},
		{name: "foreground with owned terminal and zero cost"},
		{name: "background with owned terminal and zero cost", fixture: openCodeChildTestFixture{background: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := tc.fixture
			out, err := fixture.run(t)
			evidence := out.OpenCode
			if tc.code != "" {
				var fault *model.Fault
				if !errors.As(err, &fault) || fault.Code != tc.code {
					t.Fatalf("want %s, got %v", tc.code, err)
				}
				if tc.code == "RATE_LIMIT" && (fault.UpstreamStatus != 429 || fault.RetryAfterMS != 30000 || fault.ProviderRateLimitScope != "") {
					t.Fatalf("unexpected cooldown: %+v", fault)
				}
				if out.Summary != "" || evidence.TerminalVerified || evidence.NativeChild != nil || evidence.ReportedCost != nil {
					t.Fatalf("failed run exposed success evidence: %+v", evidence)
				}
				return
			}
			if err != nil || out.Summary != "42" || !evidence.GuardVerified || !evidence.PromptSent || !evidence.TerminalVerified ||
				evidence.ReportedCost == nil || *evidence.ReportedCost != 0 || evidence.Model != "opencode/glm-5.3-flash-free" ||
				evidence.InputTokens != 15 || evidence.OutputTokens != 2 || evidence.ObservedTools["subagent"] != 1 {
				t.Fatalf("verified owned child and cost missing: %+v, %v", evidence, err)
			}
			child := evidence.NativeChild
			if child == nil || !child.ChildFinished || child.BackgroundCompleted != fixture.background ||
				child.ParentSHA256 != model.Hash([]byte(testOpenCodeParentID)) || child.ChildSHA256 != model.Hash([]byte(testOpenCodeChildID)) ||
				child.ResultSHA256 != model.Hash([]byte("42")) {
				t.Fatalf("unbound child evidence: %+v", child)
			}
			if fixture.prompts != 1 || fixture.childInterrupts != 0 || fixture.parentInterrupts != 0 {
				t.Fatalf("unexpected requests: %d prompts, %d parent interrupts, %d child interrupts", fixture.prompts, fixture.parentInterrupts, fixture.childInterrupts)
			}
		})
	}
}

const (
	testOpenCodeChildModel     = "glm-5.3-flash-free"
	testOpenCodeChildDir       = "/private/tmp/ahenk-native-child-fixture"
	testOpenCodeParentID       = "ses_parent0123456789"
	testOpenCodeChildID        = "ses_child0123456789"
	testOpenCodeChildObjective = "Compute 7 times 6"
)

type openCodeChildTestFixture struct {
	background, noNotice                       bool
	childPermission, cancelOnChildHistory      bool
	failRepeatedPermissionRead                 bool
	sessionBefore, sessionAfter                string
	parentBefore, parentAfter                  string
	historyBefore, historyAfter                string
	prompts, parentInterrupts, childInterrupts int
	childRejects                               int
	childPermissionReads                       int
}

func (f *openCodeChildTestFixture) run(t *testing.T) (execution.FixtureInput, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	const parentPath = "/api/session/" + testOpenCodeParentID
	const childPath = "/api/session/" + testOpenCodeChildID
	const modelRef = `{"id":"` + testOpenCodeChildModel + `","providerID":"opencode"}`
	const permissions = `[{"action":"*","resource":"*","effect":"deny"},{"action":"subagent","resource":"ahenk-native-child","effect":"allow"}]`
	var parentPrompt struct{ ID, Text string }
	runtime := openCodeRuntime{url: "http://127.0.0.1:54321", workdir: testOpenCodeChildDir, http: &http.Client{Transport: openCodeNativeTestTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.Method + " " + r.URL.RequestURI() {
		case "POST /api/session":
			body = `{"data":{"id":"` + testOpenCodeParentID + `","title":"Ahenk synthetic task","agent":"build","model":` + modelRef +
				`,"location":{"directory":"` + testOpenCodeChildDir + `"},"permissions":` + permissions + `}}`
		case "POST " + parentPath + "/prompt":
			f.prompts++
			if err := json.NewDecoder(r.Body).Decode(&parentPrompt); err != nil || parentPrompt.ID == "" || parentPrompt.Text == "" {
				t.Fatalf("parent prompt missing: %v", err)
			}
			body = `{}`
		case "GET " + parentPath + "/permission":
			body = `{"data":[]}`
		case "GET " + childPath + "/permission":
			f.childPermissionReads++
			if f.failRepeatedPermissionRead && f.childPermissionReads > 1 {
				return nil, errors.New("fixture permission read unavailable during cleanup")
			}
			body = `{"data":[]}`
			if f.childPermission {
				body = `{"data":[{"id":"per_0123456789abcdefghij","sessionID":"` + testOpenCodeChildID + `","action":"read"}]}`
			}
		case "POST " + childPath + "/permission/per_0123456789abcdefghij/reply":
			f.childRejects++
			var reply map[string]string
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil || len(reply) != 1 || reply["decision"] != "reject" {
				t.Fatalf("unexpected child permission response: %v, %v", reply, err)
			}
			body = `{}`
		case "GET " + parentPath + "/message?limit=10":
			status, notice := "completed", ""
			if f.background {
				status = "running"
			}
			if f.background && !f.noNotice {
				notice = `,{"type":"synthetic","id":"notice","time":{"created":5},"text":"<subagent sessionID=\"` + testOpenCodeChildID + `\" state=\"completed\" description=\"Ahenk synthetic child\">\n42\n</subagent>",` +
					`"metadata":{"source":"subagent","childID":"` + testOpenCodeChildID + `","agent":"ahenk-native-child","state":"completed"}}`
				notice += `,{"type":"assistant","id":"ack","agent":"build","model":` + modelRef + `,"time":{"created":6,"completed":7},"cost":0,"finish":"stop","content":[{"type":"text","text":"Acknowledged"}]},{"type":"idle","id":"ack-idle","outcome":"succeeded","time":{"created":8}}`
			}
			prompt, err := json.Marshal(parentPrompt.Text)
			if err != nil {
				t.Fatal(err)
			}
			body = `{"data":[{"type":"idle","outcome":"succeeded","time":{"created":4}},{"type":"assistant","agent":"build","model":` + modelRef +
				`,"time":{"created":2,"completed":3},"cost":0,"content":[{"type":"tool","name":"subagent","state":{"status":"completed",` +
				`"input":{"agent":"ahenk-native-child","description":"Ahenk synthetic child","prompt":"` + testOpenCodeChildObjective +
				`","background":` + strconv.FormatBool(f.background) + `},"metadata":{"sessionID":"` + testOpenCodeChildID + `","status":"` + status + `"}}}]},` +
				`{"type":"user","id":"` + parentPrompt.ID + `","text":` + string(prompt) + `,"time":{"created":1}}` + notice + `]}`
			body = replaceOpenCodeChildFixture(t, body, f.parentBefore, f.parentAfter)
			if f.cancelOnChildHistory {
				body = strings.Replace(body, `{"type":"idle","outcome":"succeeded","time":{"created":4}},`, "", 1)
			}
		case "GET " + childPath:
			body = `{"data":{"id":"` + testOpenCodeChildID + `","parentID":"` + testOpenCodeParentID + `","title":"Ahenk synthetic child","agent":"ahenk-native-child","model":` + modelRef +
				`,"location":{"directory":"` + testOpenCodeChildDir + `"},"permissions":` + permissions + `}}`
			body = replaceOpenCodeChildFixture(t, body, f.sessionBefore, f.sessionAfter)
		case "GET " + childPath + "/message?limit=10":
			if f.cancelOnChildHistory {
				cancel()
			}
			body = `{"data":[{"type":"idle","outcome":"succeeded","time":{"created":4}},` +
				`{"type":"assistant","agent":"ahenk-native-child","model":` + modelRef + `,"content":[{"type":"text","text":"42"}],` +
				`"finish":"stop","cost":0,"tokens":{"input":15,"output":2},"time":{"created":2,"completed":3}},` +
				`{"type":"user","id":"msg_native_child_fixture","text":"You are a subagent spawned by another session.\n` + testOpenCodeChildObjective + `","time":{"created":1}}]}`
			body = replaceOpenCodeChildFixture(t, body, f.historyBefore, f.historyAfter)
		case "POST " + parentPath + "/interrupt?resume=false":
			f.parentInterrupts++
			body = `{"interrupted":true}`
		case "POST " + childPath + "/interrupt?resume=false":
			f.childInterrupts++
			body = `{"interrupted":true}`
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	rules := []openCodeNativeRule{{Action: "*", Resource: "*", Effect: "deny"}, {Action: "subagent", Resource: "ahenk-native-child", Effect: "allow"}}
	return runtime.runChild(ctx, testOpenCodeChildModel, testOpenCodeChildObjective, rules, f.background)
}

func replaceOpenCodeChildFixture(t *testing.T, value, before, after string) string {
	t.Helper()
	if before == "" {
		return value
	}
	if !strings.Contains(value, before) {
		t.Fatalf("fixture does not contain %q", before)
	}
	return strings.Replace(value, before, after, 1)
}
