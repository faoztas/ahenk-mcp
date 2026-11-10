// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	openCodeChildAgent  = "ahenk-native-child"
	openCodeChildTitle  = "Ahenk synthetic child"
	openCodeChildPrefix = "You are a subagent spawned by another session.\n"
)

func runOpenCodeChild(ctx context.Context, dir, id, objective string, background bool) (out execution.FixtureInput, err error) {
	if err = checkOpenCodeNativeMachinePolicy(); err != nil {
		return out, err
	}
	disabled := map[string]any{}
	err = withOpenCode(ctx, dir, nil, false, func(o openCodeRuntime) error {
		models, err := o.catalog(ctx)
		if err != nil {
			return err
		}
		for _, m := range models {
			if m.Provider == "opencode" && m.ID != id {
				disabled[m.ID] = map[string]bool{"disabled": true}
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	// The catalog server has exited. Reuse the attempt lease so quarantine
	// can still detect an uncertain cleanup from either server.
	if !execution.LeaseReleased(dir) {
		return out, execution.ErrCleanup
	}
	for _, name := range []string{".worker.cleaned", ".worker.lock"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return out, err
		}
	}
	rules := []openCodeNativeRule{{Action: "*", Resource: "*", Effect: "deny"},
		{Action: "subagent", Resource: openCodeChildAgent, Effect: "allow"}}
	deny := rules[:1]
	config, err := json.Marshal(map[string]any{
		"model":     "opencode/" + id,
		"providers": map[string]any{"opencode": map[string]any{"models": disabled}},
		"agents": map[string]any{
			"build": map[string]any{"steps": 3, "permissions": rules},
			"title": map[string]bool{"disabled": true}, "summary": map[string]bool{"disabled": true},
			openCodeChildAgent: map[string]any{"mode": "subagent", "steps": 1, "permissions": deny,
				"model": "opencode/" + id, "description": "Solve one synthetic text task without tools"},
		},
		"experimental": map[string]int{"subagent_depth": 1},
		"compaction":   map[string]bool{"auto": false}, "formatter": false, "lsp": false,
		"policies": []openCodeNativeRule{
			{Action: "provider.use", Resource: "*", Effect: "deny"}, {Action: "provider.use", Resource: "opencode", Effect: "allow"},
			{Action: "integration.connect", Resource: "*", Effect: "deny"},
			{Action: "mcp.use", Resource: "*", Effect: "deny"}, {Action: "skill.use", Resource: "*", Effect: "deny"},
		},
	})
	if err != nil {
		return out, err
	}
	err = withOpenCodeWorkspace(ctx, dir, "", string(config), nil, func(o openCodeRuntime) error {
		models, err := o.catalog(ctx)
		if err != nil {
			return err
		}
		if len(models) != 1 || !nativeRouteMatches(models[0], id) || !openCodeFreeModels(models)[id] {
			return model.Error("ZERO_COST_UNVERIFIED", "OpenCode child route requires exactly one pinned free model")
		}
		if err := o.verifyNative(ctx, models, id, 3); err != nil {
			return err
		}
		for _, profile := range []struct {
			id, mode string
			steps    int
			rules    []openCodeNativeRule
		}{{openCodeNativeAgent, "primary", 3, rules}, {openCodeChildAgent, "subagent", 1, deny}} {
			var agent struct {
				Data struct {
					ID          string               `json:"id"`
					Mode        string               `json:"mode"`
					Steps       int                  `json:"steps"`
					Permissions []openCodeNativeRule `json:"permissions"`
					Model       openCodeNativeRef    `json:"model"`
				} `json:"data"`
			}
			if err := o.request(ctx, http.MethodGet, "/api/agent/"+profile.id, nil, &agent); err != nil {
				return err
			}
			a := agent.Data
			if a.ID != profile.id || a.Mode != profile.mode || a.Steps != profile.steps ||
				profile.id == openCodeChildAgent && !a.Model.matches(id) || !openCodeChildRules(a.Permissions, profile.rules) {
				fault := model.Error("POLICY_DENIED", "OpenCode native child permissions or eligibility were not verified")
				fault.ProviderReason = "OPENCODE_CHILD_AGENT_GUARD"
				return fault
			}
		}
		out, err = o.runChild(ctx, id, objective, rules, background)
		return err
	})
	return out, err
}

// The pinned release prepends defaults and appends browser denials. Its
// last matching rule wins. Require the wildcard deny to override all defaults.
func openCodeChildRules(actual, want []openCodeNativeRule) bool {
	start := slices.Index(actual, want[0])
	if start < 0 || len(actual)-start < len(want) || !slices.Equal(actual[start:start+len(want)], want) {
		return false
	}
	for _, rule := range actual[start+len(want):] {
		if rule.Effect != "deny" {
			return false
		}
	}
	return true
}

func (o openCodeRuntime) runChild(ctx context.Context, id, objective string, rules []openCodeNativeRule, background bool) (out execution.FixtureInput, result error) {
	evidence := &model.OpenCodeRunEvidence{}
	out.OpenCode = evidence
	session, err := o.startNativeSession(ctx, id, rules)
	if err != nil {
		return out, err
	}
	evidence.GuardVerified = true
	path := "/api/session/" + session.ID
	var childID string
	defer func() {
		if result != nil {
			o.stopNative(path, session.ID)
			if childID != "" {
				o.stopNative("/api/session/"+childID, childID)
			}
		}
	}()
	mode, terminal := "in foreground", "Wait for the tool result and then finish."
	if background {
		mode, terminal = "with background set to true", "Finish the parent turn after launching the child. When notified, acknowledge without tools."
	}
	prompt := "Use the native subagent tool exactly once, " + mode + ", with agent " + openCodeChildAgent +
		", description " + openCodeChildTitle + ", no model override and no sessionID. Copy the objective below verbatim into the prompt argument. " +
		terminal + " Do not use any other tools.\n" + objective
	messageID := openCodeNativeMessageID(time.Now())
	if err := o.sendNativePrompt(ctx, path, id, messageID, prompt); err != nil {
		return out, err
	}
	evidence.PromptSent = true
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		messages, err := o.childMessages(ctx, path, session.ID)
		if err != nil {
			return out, err
		}
		child, done, err := openCodeChildInvocation(messages, id, objective, messageID, prompt, background)
		if err != nil {
			return out, err
		}
		if child != "" {
			if child == session.ID || childID != "" && child != childID {
				return out, model.Error("POLICY_DENIED", "OpenCode child identity changed")
			}
			childID = child
		}
		if done {
			out.Summary, err = o.verifyChildResult(ctx, session, childID, id, objective, evidence)
			if err == nil && background {
				notice, _ := openCodeBackgroundNotice(messages, childID)
				if notice != out.Summary {
					return out, model.Error("INVALID_OUTPUT", "OpenCode background notification and child result differed")
				}
				evidence.NativeChild.BackgroundCompleted = true
			}
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
		}
	}
}

func (o openCodeRuntime) childMessages(ctx context.Context, path, session string) ([]openCodeNativeMessage, error) {
	pending, err := o.nativePermissions(ctx, path, session)
	if err != nil {
		return nil, err
	}
	if len(pending) != 0 {
		o.rejectNativePermission(path, pending[0].ID)
		return nil, model.Error("PERMISSION_DENIED", "OpenCode subagent requested an unauthorized permission")
	}
	var history struct {
		Data []openCodeNativeMessage `json:"data"`
	}
	if err := o.request(ctx, http.MethodGet, path+"/message?limit=10", nil, &history); err != nil {
		return nil, err
	}
	if history.Data == nil || len(history.Data) >= 10 {
		return nil, model.Error("INVALID_OUTPUT", "OpenCode subagent history is incomplete")
	}
	return history.Data, nil
}

func openCodeChildInvocation(messages []openCodeNativeMessage, modelID, objective, messageID, prompt string, backgroundMode ...bool) (string, bool, error) {
	var child string
	background := len(backgroundMode) == 1 && backgroundMode[0]
	var invoked, completed, user, idle bool
	for _, m := range messages {
		switch m.Type {
		case "user":
			if user || m.ID != messageID || m.Text != prompt {
				return "", false, model.Error("INVALID_OUTPUT", "OpenCode parent prompt did not match")
			}
			user = true
		case "idle":
			if m.Error != nil {
				return "", false, m.Error.error()
			}
			idle = m.Outcome == "succeeded"
		case "synthetic":
			if !background {
				return "", false, model.Error("POLICY_DENIED", "unexpected OpenCode background notification")
			}
		case "assistant":
			if m.Error != nil {
				return "", false, m.Error.error()
			}
			if m.Retry != nil {
				return "", false, m.Retry.Error.error()
			}
			if m.Agent != openCodeNativeAgent || !m.Model.matches(modelID) ||
				m.Time.Completed != 0 && (m.Cost == nil || *m.Cost != 0) {
				return "", false, model.Error("ZERO_COST_UNVERIFIED", "OpenCode parent model or zero cost did not match")
			}
			for _, part := range m.Content {
				if part.Type == "text" || part.Type == "reasoning" {
					continue
				}
				if part.Type != "tool" || part.Name != "subagent" || invoked {
					return "", false, model.Error("PERMISSION_DENIED", "OpenCode parent attempted an unexpected tool")
				}
				invoked = true
				if part.State.Status == "streaming" {
					continue
				}
				var input struct {
					Agent       string
					Description string
					Prompt      string
					Model       string
					SessionID   string
					Background  bool
				}
				if json.Unmarshal(part.State.Input, &input) != nil || input.Agent != openCodeChildAgent ||
					input.Description != openCodeChildTitle || input.Prompt != objective || input.SessionID != "" ||
					input.Background != background || input.Model != "" && input.Model != "opencode/"+modelID {
					return "", false, model.Error("POLICY_DENIED", "OpenCode child invocation exceeded its scope")
				}
				if part.State.Status == "error" {
					return "", false, model.Error("INVALID_OUTPUT", "OpenCode native subagent tool reported failure")
				}
				var metadata struct {
					SessionID string
					Status    string
				}
				_ = json.Unmarshal(part.State.Metadata, &metadata)
				if metadata.SessionID != "" && !openCodeSessionID.MatchString(metadata.SessionID) {
					return "", false, model.Error("INVALID_OUTPUT", "OpenCode child session identity is malformed")
				}
				child = metadata.SessionID
				wantStatus := "completed"
				if background {
					wantStatus = "running"
				}
				completed = part.State.Status == "completed" && metadata.Status == wantStatus && child != ""
			}
		default:
			return "", false, model.Error("POLICY_DENIED", "OpenCode parent returned unexpected session activity")
		}
	}
	if idle && (!user || !completed) {
		return child, false, model.Error("INVALID_OUTPUT", "OpenCode parent stopped without a verified native child")
	}
	if background && completed {
		notice, err := openCodeBackgroundNotice(messages, child)
		return child, user && idle && notice != "", err
	}
	return child, user && idle && completed, nil
}

func openCodeBackgroundNotice(messages []openCodeNativeMessage, child string) (string, error) {
	var result string
	for _, m := range messages {
		if m.Type != "synthetic" {
			continue
		}
		var metadata struct {
			Source  string
			ChildID string
			Agent   string
			State   string
		}
		if json.Unmarshal(m.Metadata, &metadata) != nil || metadata.Source != "subagent" || metadata.ChildID != child ||
			metadata.Agent != openCodeChildAgent || result != "" {
			return "", model.Error("INVALID_OUTPUT", "OpenCode background event did not belong to the owned child")
		}
		if metadata.State != "completed" {
			return "", model.Error("INVALID_OUTPUT", "OpenCode background child failed or was cancelled")
		}
		prefix := `<subagent sessionID="` + child + `" state="completed" description="` + openCodeChildTitle + `">` + "\n"
		text, ok := strings.CutPrefix(m.Text, prefix)
		if !ok {
			return "", model.Error("INVALID_OUTPUT", "OpenCode background result envelope did not match")
		}
		text, ok = strings.CutSuffix(text, "\n</subagent>")
		if !ok || strings.TrimSpace(text) == "" {
			return "", model.Error("INVALID_OUTPUT", "OpenCode background result was empty")
		}
		result = strings.TrimSpace(text)
	}
	return result, nil
}

func (o openCodeRuntime) verifyChildResult(ctx context.Context, parent openCodeNativeSession, child, id, objective string, evidence *model.OpenCodeRunEvidence) (string, error) {
	path := "/api/session/" + child
	var row struct{ Data openCodeNativeSession }
	if err := o.request(ctx, http.MethodGet, path, nil, &row); err != nil {
		return "", err
	}
	s := row.Data
	if s.ID != child || s.Parent != parent.ID || s.Location != parent.Location || !slices.Equal(s.Permissions, parent.Permissions) ||
		s.Agent != openCodeChildAgent || s.Title != openCodeChildTitle || !s.Model.matches(id) {
		return "", model.Error("POLICY_DENIED", "OpenCode child session ownership or permissions did not match")
	}
	messages, err := o.childMessages(ctx, path, child)
	if err != nil {
		return "", err
	}
	user := slices.IndexFunc(messages, func(m openCodeNativeMessage) bool { return m.Type == "user" })
	if user < 0 || messages[user].ID == "" {
		return "", model.Error("INVALID_OUTPUT", "OpenCode child prompt is missing")
	}
	summary, err := nativeTextCompletion(messages, messages[user].ID, id, openCodeChildPrefix+objective, openCodeChildAgent, evidence)
	if err != nil {
		return "", err
	}
	if summary == "" || model.ContainsSecret([]byte(summary)) {
		return "", model.Error("INVALID_OUTPUT", "OpenCode child result was missing or invalid")
	}
	evidence.NativeChild = &model.NativeChildEvidence{
		ParentSHA256: model.Hash([]byte(parent.ID)), ChildSHA256: model.Hash([]byte(child)), ChildFinished: true,
		ResultSHA256: model.Hash([]byte(summary)),
	}
	evidence.ObservedTools = map[string]int{"subagent": 1}
	return summary, nil
}
