// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"net/http"
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
				profile.id == openCodeChildAgent && !a.Model.matches(id, "") || !openCodeChildRules(a.Permissions, profile.rules) {
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
	var ownedChild string
	var parentPermission, childPermission string
	defer func() {
		if result != nil {
			o.stopNative(path, session.ID, parentPermission)
			if ownedChild != "" {
				o.stopNative("/api/session/"+ownedChild, ownedChild, childPermission)
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
		messages, err := o.childMessages(ctx, path, session.ID, &parentPermission)
		if err != nil {
			return out, err
		}
		child, done, parentErr := openCodeChildInvocation(messages, id, objective, messageID, prompt, background)
		if child == "" && parentErr != nil {
			return out, parentErr
		}
		if child != "" {
			if child == session.ID || ownedChild != "" && child != ownedChild {
				return out, model.Error("POLICY_DENIED", "OpenCode child identity changed")
			}
			owned, err := o.verifyChildOwnership(ctx, session, child, id)
			if err != nil {
				return out, err
			}
			if !owned {
				ownedChild = ""
				return out, model.Error("POLICY_DENIED", "OpenCode child session ownership or permissions did not match")
			}
			ownedChild = child
			// Poll owned child history even while waiting for a background
			// notification or after a generic failure notification, so
			// the child's typed upstream fault takes precedence.
			verified := *evidence
			summary, err := o.verifyChildResult(ctx, session, child, id, objective, &verified, &childPermission)
			if err != nil {
				return out, err
			}
			if parentErr != nil {
				return out, parentErr
			}
			if done {
				if summary == "" {
					return out, model.Error("INVALID_OUTPUT", "OpenCode child did not reach an independent terminal result")
				}
				if background {
					notice, err := openCodeBackgroundNotice(messages, child)
					if err != nil {
						return out, err
					}
					if notice != summary {
						return out, model.Error("INVALID_OUTPUT", "OpenCode background notification and child result differed")
					}
					verified.NativeChild.BackgroundCompleted = true
				}
				*evidence = verified
				out.Summary = summary
				return out, nil
			}
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
		}
	}
}

func (o openCodeRuntime) childMessages(ctx context.Context, path, session string, permission *string) ([]openCodeNativeMessage, error) {
	pending, err := o.nativePermissions(ctx, path, session)
	if err != nil {
		return nil, err
	}
	if len(pending) != 0 {
		// Retain the checked ID so shutdown can reject it without another read.
		*permission = pending[0].ID
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
	for _, m := range history.Data {
		if m.SessionID != "" && m.SessionID != session {
			return nil, model.Error("INVALID_OUTPUT", "OpenCode event belongs to another session")
		}
	}
	return history.Data, nil
}

func openCodeChildInvocation(messages []openCodeNativeMessage, modelID, objective, messageID, prompt string, backgroundMode ...bool) (string, bool, error) {
	var child string
	background := len(backgroundMode) == 1 && backgroundMode[0]
	var invoked, completed, idle bool
	var user, launch, foregroundIdle *openCodeNativeMessage
	var toolErr error
	for i := range messages {
		m := &messages[i]
		switch m.Type {
		case "user":
			if user != nil || m.ID != messageID || m.Text != prompt {
				return "", false, model.Error("INVALID_OUTPUT", "OpenCode parent prompt did not match")
			}
			user = m
		case "idle":
			if !background {
				if foregroundIdle != nil {
					toolErr = model.Error("INVALID_OUTPUT", "OpenCode parent has multiple terminal events")
				}
				foregroundIdle = m
			} else if m.Error != nil {
				return "", false, m.Error.error()
			}
			idle = true
			if m.Outcome != "succeeded" {
				toolErr = model.Error("INVALID_OUTPUT", "OpenCode parent did not finish successfully")
			}
		case "synthetic":
			if !background {
				return "", false, model.Error("POLICY_DENIED", "unexpected OpenCode background notification")
			}
		case "assistant":
			if !background {
				if launch != nil || m.ParentID != "" && m.ParentID != messageID {
					return "", false, model.Error("INVALID_OUTPUT", "OpenCode foreground assistant does not identify a unique launch turn")
				}
				launch = m
			}
			if m.Error != nil {
				return "", false, m.Error.error()
			}
			if m.Retry != nil {
				return "", false, m.Retry.Error.error()
			}
			if m.Agent != openCodeNativeAgent || !m.Model.matches(modelID, "") ||
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
				var metadata struct {
					SessionID string
					Status    string
				}
				if len(part.State.Metadata) != 0 && json.Unmarshal(part.State.Metadata, &metadata) != nil {
					return "", false, model.Error("INVALID_OUTPUT", "OpenCode child invocation metadata is malformed")
				}
				if metadata.SessionID != "" && !openCodeSessionID.MatchString(metadata.SessionID) {
					return "", false, model.Error("INVALID_OUTPUT", "OpenCode child session identity is malformed")
				}
				child = metadata.SessionID
				if part.State.Status == "error" {
					toolErr = model.Error("INVALID_OUTPUT", "OpenCode native subagent tool reported failure")
				}
				wantStatus := "completed"
				if background {
					wantStatus = "running"
				}
				completed = part.State.Status == "completed" && metadata.Status == wantStatus && child != "" && m.Time.Completed > 0
			}
		default:
			return "", false, model.Error("POLICY_DENIED", "OpenCode parent returned unexpected session activity")
		}
	}
	if user == nil {
		if idle {
			return "", false, model.Error("INVALID_OUTPUT", "OpenCode parent stopped without the owned prompt")
		}
		return "", false, nil
	}
	if foregroundIdle != nil {
		if launch == nil || !idleReferences(foregroundIdle, launch) || !nativeTurnTimesMatch(user, launch, foregroundIdle) {
			return child, false, model.Error("INVALID_OUTPUT", "OpenCode parent idle does not match its launch turn")
		}
		if foregroundIdle.Error != nil {
			return child, false, foregroundIdle.Error.error()
		}
	}
	if toolErr != nil {
		return child, false, toolErr
	}
	if idle && !completed {
		return child, false, model.Error("INVALID_OUTPUT", "OpenCode parent stopped without a verified native child")
	}
	if background && completed {
		notice, err := openCodeBackgroundNotice(messages, child)
		if err != nil || notice == "" {
			return child, false, err
		}
		done, err := backgroundAcknowledged(messages)
		return child, done, err
	}
	return child, idle && completed, nil
}

// The API can return history newest first. Correlate turns by their times
// within the owned parent history, rejecting ambiguous or foreign activity.
func backgroundAcknowledged(messages []openCodeNativeMessage) (bool, error) {
	var user, launch, notice, ack *openCodeNativeMessage
	var idles []*openCodeNativeMessage
	seen := map[string]bool{}
	invalid := model.Error("INVALID_OUTPUT", "OpenCode background acknowledgement is ambiguous")
	for i := range messages {
		m := &messages[i]
		if m.ID != "" {
			if seen[m.ID] {
				return false, invalid
			}
			seen[m.ID] = true
		}
		switch m.Type {
		case "user":
			user = m
		case "synthetic":
			notice = m
		case "idle":
			idles = append(idles, m)
		case "assistant":
			if slices.ContainsFunc(m.Content, func(p openCodeNativePart) bool { return p.Type == "tool" }) {
				launch = m
				continue
			}
			if ack != nil {
				return false, invalid
			}
			ack = m
		}
	}
	if user == nil || launch == nil || notice == nil || launch.ParentID != "" && launch.ParentID != user.ID ||
		user.Time.Created <= 0 || launch.Time.Created < user.Time.Created || launch.Time.Completed < launch.Time.Created ||
		notice.Time.Created <= launch.Time.Completed {
		return false, invalid
	}
	if ack == nil || ack.Time.Completed == 0 {
		return false, nil
	}
	if ack.ParentID != "" && ack.ParentID != notice.ID || ack.Time.Created <= notice.Time.Created || ack.Time.Completed < ack.Time.Created || ack.Finish != "stop" ||
		!slices.ContainsFunc(ack.Content, func(p openCodeNativePart) bool { return p.Type == "text" && strings.TrimSpace(p.Text) != "" }) {
		return false, invalid
	}
	var launchIdle, ackIdle bool
	for _, idle := range idles {
		switch {
		case idle.Time.Created >= launch.Time.Completed && idle.Time.Created < notice.Time.Created && !launchIdle:
			if !idleReferences(idle, launch) {
				return false, invalid
			}
			launchIdle = true
		case idle.Time.Created >= ack.Time.Completed && !ackIdle:
			if !idleReferences(idle, ack) {
				return false, invalid
			}
			ackIdle = true
		default:
			return false, invalid
		}
		if idle.Outcome != "succeeded" {
			return false, invalid
		}
	}
	return ackIdle, nil
}

func idleReferences(idle, assistant *openCodeNativeMessage) bool {
	return (idle.MessageID == "" || idle.MessageID == assistant.ID) && (idle.ParentID == "" || idle.ParentID == assistant.ID)
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

func (o openCodeRuntime) verifyChildOwnership(ctx context.Context, parent openCodeNativeSession, child, id string) (bool, error) {
	var row struct{ Data openCodeNativeSession }
	if err := o.request(ctx, http.MethodGet, "/api/session/"+child, nil, &row); err != nil {
		return false, err
	}
	s := row.Data
	return s.ID == child && s.Parent == parent.ID && s.Location == parent.Location && slices.Equal(s.Permissions, parent.Permissions) &&
		s.Agent == openCodeChildAgent && s.Title == openCodeChildTitle && s.Model.matches(id, ""), nil
}

func (o openCodeRuntime) verifyChildResult(ctx context.Context, parent openCodeNativeSession, child, id, objective string, evidence *model.OpenCodeRunEvidence, permission *string) (string, error) {
	messages, err := o.childMessages(ctx, "/api/session/"+child, child, permission)
	if err != nil {
		return "", err
	}
	user := slices.IndexFunc(messages, func(m openCodeNativeMessage) bool { return m.Type == "user" })
	var messageID string
	if user >= 0 {
		messageID = messages[user].ID
		if messageID == "" {
			return "", model.Error("INVALID_OUTPUT", "OpenCode child prompt identity is missing")
		}
	}
	summary, err := nativeTextCompletion(messages, messageID, id, openCodeChildPrefix+objective, openCodeChildAgent, evidence)
	if err != nil || summary == "" {
		return "", err
	}
	if model.ContainsSecret([]byte(summary)) {
		return "", model.Error("INVALID_OUTPUT", "OpenCode child result was invalid")
	}
	evidence.NativeChild = &model.NativeChildEvidence{
		ParentSHA256: model.Hash([]byte(parent.ID)), ChildSHA256: model.Hash([]byte(child)), ChildFinished: true,
		ResultSHA256: model.Hash([]byte(summary)),
	}
	evidence.ObservedTools = map[string]int{"subagent": 1}
	return summary, nil
}
