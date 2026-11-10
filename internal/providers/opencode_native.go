// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	openCodeNativeModel = "opencode/glm-5.3-flash-free"
	openCodeNativeURL   = model.OpenCodeFreeEndpoint
	openCodeNativeSDK   = "@opencode/ai/providers/openai-compatible"
	openCodeNativeAgent = "build"
	// The native step counter begins at one. A cap of one changes the first
	// provider request to a final-step request and disables tool selection.
	openCodeNativeConfig = `{"agents":{"build":{"steps":2},"title":{"disabled":true},"summary":{"disabled":true}},"compaction":{"auto":false},"policies":[{"action":"skill.use","resource":"*","effect":"deny"}]}`
)

var (
	openCodeSessionID    = regexp.MustCompile(`^ses_[A-Za-z0-9]{12,100}$`)
	openCodePermissionID = regexp.MustCompile(`^per_[A-Za-z0-9]{12,100}$`)
)

func openCodeTransportMatches(qualifiedModel, transport string) bool {
	switch model.OpenCodeTransportID(transport) {
	case model.OpenCodeStateless:
		return true
	case model.OpenCodeNativeReadAsk:
		return qualifiedModel == openCodeNativeModel
	default:
		return false
	}
}

func checkOpenCodeNativeMachinePolicy() error {
	account, err := user.Current()
	if err != nil || account.Username == "" || filepath.Base(account.Username) != account.Username {
		return model.Error("POLICY_DENIED", "OpenCode managed configuration could not be excluded")
	}
	const prefs = "/Library/Managed Preferences"
	for _, path := range []string{"/Library/Application Support/opencode",
		filepath.Join(prefs, "ai.opencode.managed.plist"), filepath.Join(prefs, account.Username, "ai.opencode.managed.plist")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return model.Error("POLICY_DENIED", "OpenCode managed configuration could not be excluded")
		}
	}
	return nil
}

type openCodeNativeRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

type openCodeNativeRef struct {
	Provider string `json:"providerID"`
	ID       string `json:"id"`
	Variant  string `json:"variant,omitempty"`
}

func (m openCodeNativeRef) matches(id string) bool {
	return m.Provider == "opencode" && m.ID == id && (m.Variant == "" || m.Variant == "default")
}

type openCodeNativeLocation struct {
	Directory string `json:"directory"`
}

func nativeRouteMatches(m openCodeCatalogModel, id string) bool {
	return m.Provider == "opencode" && m.ID == id && m.Type == "chat" && m.ModelID == id &&
		m.Package == openCodeNativeSDK && m.Settings.BaseURL == openCodeNativeURL &&
		len(m.Headers) == 0 && len(m.Body) == 0
}

func (o openCodeRuntime) verifyNative(ctx context.Context, models []openCodeCatalogModel, id string, steps int) error {
	if !slices.ContainsFunc(models, func(m openCodeCatalogModel) bool { return nativeRouteMatches(m, id) }) {
		return model.Error("ZERO_COST_UNVERIFIED", "OpenCode native model route differs from the audited free route")
	}
	var agents struct {
		Location openCodeNativeLocation `json:"location"`
		Data     []struct {
			ID    string `json:"id"`
			Steps int    `json:"steps"`
		} `json:"data"`
	}
	if err := o.request(ctx, http.MethodGet, "/api/agent", nil, &agents); err != nil {
		return err
	}
	build := 0
	for _, agent := range agents.Data {
		switch agent.ID {
		case "title", "summary":
			return model.Error("POLICY_DENIED", "OpenCode native helper agents are enabled")
		case openCodeNativeAgent:
			if agent.Steps != steps {
				return model.Error("POLICY_DENIED", "OpenCode native agent profile was not verified")
			}
			build++
		}
	}
	if agents.Location.Directory != o.workdir || build != 1 {
		return model.Error("POLICY_DENIED", "OpenCode native agent profile was not verified")
	}
	for _, name := range []string{"title", "summary"} {
		var helper struct{}
		var fault *model.Fault
		err := o.request(ctx, http.MethodGet, "/api/agent/"+name, nil, &helper)
		if !errors.As(err, &fault) || fault.HTTPStatus != http.StatusNotFound {
			return model.Error("POLICY_DENIED", "OpenCode native helper agent could not be excluded")
		}
	}
	for _, path := range []string{"/api/permission/saved", "/api/skill", "/api/mcp"} {
		var inventory struct {
			Data []struct{} `json:"data"`
		}
		if err := o.request(ctx, http.MethodGet, path, nil, &inventory); err != nil {
			return err
		}
		if inventory.Data == nil || len(inventory.Data) != 0 {
			return model.Error("POLICY_DENIED", "OpenCode native isolation inventory was not empty")
		}
	}
	var integration struct {
		Data struct {
			ID          string     `json:"id"`
			Connections []struct{} `json:"connections"`
		} `json:"data"`
	}
	if err := o.request(ctx, http.MethodGet, "/api/integration/opencode", nil, &integration); err != nil {
		return err
	}
	if integration.Data.ID != "opencode" || integration.Data.Connections == nil || len(integration.Data.Connections) != 0 {
		return model.Error("AUTH_REQUIRED", "OpenCode native anonymous integration was not verified")
	}
	return nil
}

func openCodeNativeMessageID(now time.Time) string {
	const mask = (1 << 48) - 1
	return fmt.Sprintf("msg_%012x%s", (uint64(now.UnixMilli())<<12|1)&mask, rand.Text()[:14])
}

func (o openCodeRuntime) runNative(ctx context.Context, models []openCodeCatalogModel, id, prompt string, followUp ...string) (execution.FixtureInput, error) {
	return o.runNativeSession(ctx, models, id, prompt, nil, followUp...)
}

func (o openCodeRuntime) runNativeSession(ctx context.Context, models []openCodeCatalogModel, id, prompt string, scope *openCodeWriteScope, followUp ...string) (out execution.FixtureInput, result error) {
	if len(followUp) > 1 || len(followUp) == 1 && scope != nil {
		return out, model.Error("POLICY_DENIED", "invalid OpenCode continuation policy")
	}
	evidence := &model.OpenCodeRunEvidence{}
	out.OpenCode = evidence
	steps, timeout := 2, 45*time.Second
	rules := []openCodeNativeRule{{Action: "*", Resource: "*", Effect: "deny"}, {Action: "read", Resource: "*", Effect: "ask"}}
	if scope != nil {
		steps, timeout, rules = openCodeWriterSteps, 120*time.Second, scope.rules
	}
	if err := o.verifyNative(ctx, models, id, steps); err != nil {
		return out, err
	}
	session, err := o.startNativeSession(ctx, id, rules)
	if err != nil {
		return out, err
	}
	evidence.GuardVerified = true
	path := "/api/session/" + session.ID
	defer func() {
		if result != nil {
			o.stopNative(path, session.ID)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var history *openCodeTurnHistory
	if len(followUp) == 1 {
		history = &openCodeTurnHistory{}
		evidence.Session = &model.SessionEvidence{IDHash: model.Hash([]byte(session.ID))}
	}
	var inputTokens, outputTokens, reasoningTokens int64
	for _, text := range append([]string{prompt}, followUp...) {
		messageID := openCodeNativeMessageID(time.Now())
		if err := o.sendNativePrompt(ctx, path, id, messageID, text); err != nil {
			return out, err
		}
		evidence.PromptSent = true
		evidence.TerminalVerified = false
		out, result = o.waitNative(ctx, path, session.ID, messageID, id, text, scope, evidence, history)
		if result != nil {
			return out, result
		}
		if history != nil {
			if model.ContainsSecret([]byte(out.Summary)) {
				return out, model.Error("INVALID_OUTPUT", "OpenCode continuation returned sensitive output")
			}
			inputTokens += evidence.InputTokens
			outputTokens += evidence.OutputTokens
			reasoningTokens += evidence.ReasoningTokens
			evidence.Session.Turns = append(evidence.Session.Turns, model.SessionTurn{
				ReplySHA256: model.Hash([]byte(out.Summary)), InputTokens: evidence.InputTokens, OutputTokens: evidence.OutputTokens,
			})
		}
	}
	if history != nil {
		evidence.InputTokens, evidence.OutputTokens, evidence.ReasoningTokens = inputTokens, outputTokens, reasoningTokens
	}
	return out, nil
}

type openCodeNativeSession struct {
	ID          string                 `json:"id"`
	Title       string                 `json:"title"`
	Agent       string                 `json:"agent"`
	Model       openCodeNativeRef      `json:"model"`
	Location    openCodeNativeLocation `json:"location"`
	Permissions []openCodeNativeRule   `json:"permissions"`
	Parent      string                 `json:"parentID"`
}

func (o openCodeRuntime) startNativeSession(ctx context.Context, id string, rules []openCodeNativeRule) (openCodeNativeSession, error) {
	ref := openCodeNativeRef{Provider: "opencode", ID: id}
	location := openCodeNativeLocation{Directory: o.workdir}
	const title = "Ahenk synthetic task"
	var created struct {
		Data openCodeNativeSession `json:"data"`
	}
	if err := o.request(ctx, http.MethodPost, "/api/session", map[string]any{
		"title": title, "agent": openCodeNativeAgent, "model": ref, "location": location, "permissions": rules,
	}, &created); err != nil {
		return openCodeNativeSession{}, err
	}
	session := created.Data
	if !openCodeSessionID.MatchString(session.ID) || session.Title != title || session.Agent != openCodeNativeAgent ||
		!session.Model.matches(id) || session.Location != location || !slices.Equal(session.Permissions, rules) || session.Parent != "" {
		return openCodeNativeSession{}, model.Error("POLICY_DENIED", "OpenCode native session did not confirm its isolation and permissions")
	}
	return session, nil
}

func (o openCodeRuntime) sendNativePrompt(ctx context.Context, path, id, messageID, text string) error {
	empty := []any{}
	return o.request(ctx, http.MethodPost, path+"/prompt", map[string]any{
		"id": messageID, "delivery": "steer", "text": text, "files": empty, "agents": empty, "skills": empty,
		"metadata": map[string]any{"displayText": text, "attachments": empty, "comments": empty, "agent": openCodeNativeAgent,
			"model": map[string]string{"providerID": "opencode", "modelID": id}},
	}, nil)
}

type openCodeNativePermission struct {
	ID      string `json:"id"`
	Session string `json:"sessionID"`
}

func (o openCodeRuntime) nativePermissions(ctx context.Context, path, sessionID string) ([]openCodeNativePermission, error) {
	var result struct {
		Data []openCodeNativePermission `json:"data"`
	}
	if err := o.request(ctx, http.MethodGet, path+"/permission", nil, &result); err != nil {
		return nil, err
	}
	if result.Data == nil || slices.ContainsFunc(result.Data, func(p openCodeNativePermission) bool {
		return p.Session != sessionID || !openCodePermissionID.MatchString(p.ID)
	}) {
		return nil, model.Error("POLICY_DENIED", "OpenCode native permission state could not be verified")
	}
	return result.Data, nil
}

func (o openCodeRuntime) stopNative(path, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	pending, err := o.nativePermissions(ctx, path, sessionID)
	cancel()
	if err == nil && len(pending) != 0 {
		o.rejectNativePermission(path, pending[0].ID)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = o.request(ctx, http.MethodPost, path+"/interrupt?resume=false", nil, nil)
}

func (o openCodeRuntime) rejectNativePermission(path, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = o.request(ctx, http.MethodPost, path+"/permission/"+requestID+"/reply", map[string]string{"decision": "reject"}, nil)
}

type openCodeNativePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status   string          `json:"status"`
		Metadata json.RawMessage `json:"metadata"`
		Input    json.RawMessage `json:"input"`
	} `json:"state"`
}

func (p *openCodeNativePart) UnmarshalJSON(data []byte) error {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return err
	}
	*p = openCodeNativePart{Type: kind.Type}
	if kind.Type != "text" && kind.Type != "tool" {
		return nil
	}
	type textPart openCodeNativePart
	return json.Unmarshal(data, (*textPart)(p))
}

type openCodeNativeFault struct {
	rawRetry   json.RawMessage
	RetryAt    json.RawMessage `json:"retryAt,omitempty"`
	ResetAt    json.RawMessage `json:"resetAt,omitempty"`
	RateScope  string          `json:"rateLimitScope,omitempty"`
	Status     int             `json:"status"`
	RetryAfter int64           `json:"retryAfterMs"`
}

func (f *openCodeNativeFault) UnmarshalJSON(data []byte) error {
	type wire openCodeNativeFault
	var fields struct {
		*wire
		RawRetry json.RawMessage `json:"retryAfterMs"`
	}
	fields.wire = (*wire)(f)
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	f.rawRetry = fields.RawRetry
	return nil
}

func (f openCodeNativeFault) error() error {
	retry := f.rawRetry
	if len(retry) == 0 && f.RetryAfter != 0 {
		retry, _ = json.Marshal(f.RetryAfter)
	}
	body, _ := json.Marshal(struct {
		Tag       string          `json:"_tag"`
		RateScope string          `json:"rateLimitScope,omitempty"`
		Status    int             `json:"status"`
		Retry     json.RawMessage `json:"retryAfterMs,omitempty"`
		RetryAt   json.RawMessage `json:"retryAt,omitempty"`
		ResetAt   json.RawMessage `json:"resetAt,omitempty"`
	}{"ProviderRequestError", f.RateScope, f.Status, retry, f.RetryAt, f.ResetAt})
	return openCodeError(0, body)
}

type openCodeNativeMessage struct {
	Metadata json.RawMessage      `json:"metadata"`
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Agent    string               `json:"agent"`
	Model    openCodeNativeRef    `json:"model"`
	Text     string               `json:"text"`
	Content  []openCodeNativePart `json:"content"`
	Finish   string               `json:"finish"`
	Outcome  string               `json:"outcome"`
	Cost     *float64             `json:"cost"`
	Error    *openCodeNativeFault `json:"error"`
	Retry    *struct {
		Error openCodeNativeFault `json:"error"`
	} `json:"retry"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
	} `json:"tokens"`
}

type openCodeTurnHistory struct {
	verified map[string]string
	idle     string
}

func (h *openCodeTurnHistory) next(messages []openCodeNativeMessage) ([]openCodeNativeMessage, error) {
	if h == nil || len(h.verified) == 0 {
		return messages, nil
	}
	current := make([]openCodeNativeMessage, 0, len(messages))
	seen := map[string]bool{}
	for _, m := range messages {
		if digest, ok := h.verified[m.ID]; ok && m.Type != "idle" {
			if seen[m.ID] || digest != model.Digest(m) {
				return nil, model.Error("POLICY_DENIED", "OpenCode session history changed between turns")
			}
			seen[m.ID] = true
			continue
		}
		if m.Type != "idle" || h.idle != model.Digest(m) {
			current = append(current, m)
		}
	}
	if len(seen) != len(h.verified) {
		return nil, model.Error("POLICY_DENIED", "OpenCode continuation did not preserve the verified history")
	}
	return current, nil
}

func (h *openCodeTurnHistory) pin(messages []openCodeNativeMessage) error {
	if h == nil || h.verified != nil {
		return nil
	}
	h.verified = make(map[string]string)
	for _, m := range messages {
		if m.Type == "idle" {
			h.idle = model.Digest(m)
			continue
		}
		if !model.ValidID(m.ID) || h.verified[m.ID] != "" {
			return model.Error("INVALID_OUTPUT", "OpenCode session history has no unique message identity")
		}
		h.verified[m.ID] = model.Digest(m)
	}
	return nil
}

func (o openCodeRuntime) waitNative(ctx context.Context, path, sessionID, messageID, id, prompt string, scope *openCodeWriteScope, evidence *model.OpenCodeRunEvidence, history *openCodeTurnHistory) (execution.FixtureInput, error) {
	out := execution.FixtureInput{OpenCode: evidence}
	limit := 10
	if scope != nil {
		limit = openCodeWriterSteps + 6
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := o.nativePermissions(ctx, path, sessionID)
		if err != nil {
			return out, err
		}
		if len(pending) != 0 {
			o.rejectNativePermission(path, pending[0].ID)
			return out, model.Error("PERMISSION_DENIED", "OpenCode native requested a tool permission")
		}
		var messages struct {
			Data []openCodeNativeMessage `json:"data"`
		}
		if err := o.request(ctx, http.MethodGet, fmt.Sprintf("%s/message?limit=%d", path, limit), nil, &messages); err != nil {
			return out, err
		}
		if messages.Data == nil || len(messages.Data) >= limit {
			return out, model.Error("INVALID_OUTPUT", "OpenCode native returned incomplete session history")
		}
		var summary string
		if scope == nil {
			current, historyErr := history.next(messages.Data)
			if historyErr != nil {
				return out, historyErr
			}
			summary, err = nativeCompletion(current, messageID, id, prompt, evidence)
			if err == nil && summary != "" {
				err = history.pin(current)
			}
		} else {
			summary, err = nativeWriterCompletion(messages.Data, messageID, id, prompt, *scope, evidence)
		}
		if err != nil || summary != "" {
			out.Summary = summary
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ticker.C:
		}
	}
}

func nativeCompletion(messages []openCodeNativeMessage, messageID, id, prompt string, evidence *model.OpenCodeRunEvidence) (string, error) {
	return nativeTextCompletion(messages, messageID, id, prompt, openCodeNativeAgent, evidence)
}

func nativeTextCompletion(messages []openCodeNativeMessage, messageID, id, prompt, agent string, evidence *model.OpenCodeRunEvidence) (string, error) {
	var input, answer, idle *openCodeNativeMessage
	for i := range messages {
		m := &messages[i]
		switch {
		case m.Type == "user" && input == nil && m.ID == messageID && m.Text == prompt:
			input = m
		case m.Type == "assistant" && !m.Model.matches(id):
			if m.Model.Provider == "opencode" && model.ValidID(m.Model.ID) {
				evidence.Model = "opencode/" + m.Model.ID
			}
			fault := model.Error("POLICY_DENIED", "OpenCode native returned a different model or variant")
			fault.ProviderReason = "MODEL_ID_MISMATCH"
			return "", fault
		case m.Type == "assistant" && answer == nil && m.Agent == agent && m.Model.matches(id):
			answer = m
		case m.Type == "idle" && idle == nil:
			idle = m
		default:
			return "", model.Error("POLICY_DENIED", "OpenCode native returned unexpected session activity")
		}
	}
	if answer == nil {
		if idle != nil {
			if idle.Error != nil {
				return "", idle.Error.error()
			}
			return "", model.Error("INVALID_OUTPUT", "OpenCode native became idle without an assistant message")
		}
		return "", nil
	}
	var text strings.Builder
	for _, part := range answer.Content {
		switch part.Type {
		case "text":
			text.WriteString(part.Text)
		case "reasoning":
		default:
			return "", model.Error("PERMISSION_DENIED", "OpenCode native returned an unexpected tool or content event")
		}
	}
	if answer.Retry != nil {
		return "", answer.Retry.Error.error()
	}
	if answer.Error != nil {
		return "", answer.Error.error()
	}
	if idle != nil && idle.Error != nil {
		return "", idle.Error.error()
	}
	if answer.Time.Completed == 0 || idle == nil {
		return "", nil
	}
	summary := strings.TrimSpace(text.String())
	if input == nil || input.Time.Created == 0 || answer.Time.Created < input.Time.Created ||
		answer.Time.Completed < answer.Time.Created || idle.Time.Created < answer.Time.Completed ||
		answer.Finish != "stop" || idle.Outcome != "succeeded" || summary == "" ||
		answer.Tokens.Input <= 0 || answer.Tokens.Output <= 0 || answer.Tokens.Reasoning < 0 {
		return "", model.Error("INVALID_OUTPUT", "OpenCode native terminal text and usage could not be verified")
	}
	if answer.Cost == nil || *answer.Cost != 0 {
		return "", model.Error("ZERO_COST_UNVERIFIED", "OpenCode native did not report zero cost")
	}
	evidence.Model = "opencode/" + answer.Model.ID
	evidence.TerminalVerified, evidence.NativeFinish = true, answer.Finish
	evidence.InputTokens, evidence.OutputTokens, evidence.ReasoningTokens = answer.Tokens.Input, answer.Tokens.Output, answer.Tokens.Reasoning
	evidence.ReportedCost = answer.Cost
	return summary, nil
}
