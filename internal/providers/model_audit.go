// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"cmp"
	"context"
	v1json "encoding/json"
	json "encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const auditPrompt = "For the Go statements x := 2; x *= 3, return only the JSON object with the final integer in the field value. No Markdown."

type AuditCooldown func(modelID string, now time.Time) *time.Time

func reuseAudit(row *model.ModelAudit, previous []model.ModelAudit) bool {
	for _, prior := range previous {
		if prior.Provider != row.Provider || prior.Model != row.Model || prior.Binary != row.Binary ||
			(prior.Inference == "NOT_RUN" && prior.LatencyMS == 0) {
			continue
		}
		if prior.Reason == "OPENCODE_UPSTREAM_HTTP_400" {
			prior.Inference = "MODEL_UNAVAILABLE"
		}
		prior.Source = row.Source
		if row.Name != "" {
			prior.Name = row.Name
		}
		if row.Pricing != nil {
			prior.Pricing = row.Pricing
		}
		prior.Context, prior.ToolMetadata = row.Context, row.ToolMetadata
		*row = prior
		return true
	}
	return false
}

func newAudit(provider, id, binary, source, protocol string) model.ModelAudit {
	return model.ModelAudit{Provider: provider, Model: id, Binary: binary, Source: source, Interface: protocol,
		Cost: "UNVERIFIED", QuotaState: "UNKNOWN", Inference: "NOT_RUN", Coding: "NOT_TESTED", Review: "NOT_TESTED"}
}

func DiscoverAGY(ctx context.Context, cfg model.ProviderConfig, dir string) ([]model.ModelAudit, error) {
	fp, err := verifyAGY(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Path, "models")
	stdout, _, err := runUsage(ctx, cmd, dir)
	if err != nil {
		return nil, model.Error("MODEL_UNAVAILABLE", "AGY model discovery did not complete")
	}
	rows := parseAGYModels(string(stdout), fp)
	if len(rows) == 0 {
		return nil, model.Error("MODEL_UNAVAILABLE", "AGY model discovery returned no slugs")
	}
	return rows, nil
}

func parseAGYModels(output, fingerprint string) []model.ModelAudit {
	seen := map[string]bool{}
	var rows []model.ModelAudit
	for line := range strings.SplitSeq(output, "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !model.ValidID(id) || seen[id] {
			continue
		}
		row := newAudit("agy", id, fingerprint, "agy models", "headless_json")
		name = strings.TrimSpace(name)
		if len(name) <= 160 && !model.ContainsSecret([]byte(name)) && !strings.ContainsAny(name, "@/") {
			row.Name = name
		}
		seen[id] = true
		row.Family = AGYModelFamily(id)
		row.QuotaBuckets = AGYFamilyBuckets(row.Family)
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			if strings.HasSuffix(id, "-"+effort) {
				row.Effort = effort
			}
		}
		if AGYIdentityMatches(id, fingerprint) {
			row.Coding, row.Review = "PASS_M2A", "PASS_M2A"
		}
		rows = append(rows, row)
	}
	return rows
}

func AuditAGY(ctx context.Context, cfg model.ProviderConfig, live bool, previous []model.ModelAudit) ([]model.ModelAudit, error) {
	dir, err := os.MkdirTemp("", "ahenk-audit-agy-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	rows, err := DiscoverAGY(ctx, cfg, dir)
	if err != nil || !live {
		return rows, err
	}
	stopped := map[string]string{}
	for _, prior := range previous {
		if prior.Provider == "agy" && prior.Binary == cfg.ExpectedBinarySHA256 && prior.Family != "" {
			if prior.Inference == "RATE_LIMITED" || prior.Inference == "QUOTA_EXHAUSTED" || prior.Reason == "QUOTA_RESERVE" {
				stopped[prior.Family] = prior.Inference
			}
		}
	}
	for i := range rows {
		row := &rows[i]
		if reuseAudit(row, previous) {
			continue
		}
		if reason := stopped[row.Family]; reason != "" {
			row.Inference, row.Reason = reason, "SHARED_BUCKET_PROBES_STOPPED"
			continue
		}
		if !cfg.EnableSynthetic || row.Binary != agy153Fingerprint || row.Family == "" {
			row.Inference, row.Reason = "COST_UNVERIFIED", "NO_VERIFIED_SUBSCRIPTION_ROUTE"
			continue
		}
		if ctx.Err() != nil {
			row.Reason = "AUDIT_CANCELLED"
			continue
		}
		work, err := os.MkdirTemp(dir, "probe-")
		if err != nil {
			return rows, err
		}
		cfg.Model = row.Model
		probe, cancel := context.WithTimeout(ctx, 40*time.Second)
		start := time.Now()
		out, err := superviseAGY(probe, work, agyRequest{Config: cfg, Audited: true,
			Task: model.Task{Mode: "read_only", DataClass: "synthetic", ExecutionProfile: "restricted", Objective: auditPrompt}})
		cancel()
		if errors.Is(err, execution.ErrCleanup) {
			keep = true
			row.Reason = "CLEANUP_UNCONFIRMED"
			return rows, err
		}
		if out.AGY != nil {
			e := out.AGY
			row.Actual, row.Terminal = e.Model, e.TerminalVerified
			if e.PromptSent {
				row.LatencyMS = time.Since(start).Milliseconds()
			}
			row.InputTokens, row.OutputTokens, row.TotalTokens = e.InputTokens, e.OutputTokens, e.TotalTokens
			if e.SubscriptionQuotaVerified {
				row.Cost, row.QuotaState, row.RuntimeCost = "SUBSCRIPTION_PRECHECK", "ABOVE_RESERVE", "UNKNOWN"
			}
		}
		completeAudit(row, out.Summary, err)
		switch row.Inference {
		case "RATE_LIMITED", "QUOTA_EXHAUSTED":
			stopped[row.Family] = row.Inference
		case "NOT_RUN":
			if row.Reason == "QUOTA_RESERVE" {
				stopped[row.Family] = row.Inference
				row.QuotaState = "RESERVED"
			}
		}
		if errors.Is(err, os.ErrPermission) {
			return rows, err
		}
	}
	return rows, nil
}

type publicAuditModel struct {
	ID, Name, Type, Status, Family string
	Capabilities                   map[string]v1json.RawMessage
	Limit                          struct{ Context int }
	Cost                           struct {
		Text struct {
			Input struct {
				Price *float64
				Cache struct{ Read, Write *float64 }
			}
			Output struct{ Price *float64 }
		}
	}
}

// Keep catalog-only candidates in the inventory while admitting inference
// only through the installed anonymous adapter.
func publicAuditModels(data []byte) map[string]publicAuditModel {
	var catalog struct {
		OpenCode struct{ Models map[string]v1json.RawMessage } `json:"opencode"`
	}
	if v1json.Unmarshal(data, &catalog) != nil {
		return nil
	}
	models := map[string]publicAuditModel{}
	for id, raw := range catalog.OpenCode.Models {
		var entry publicAuditModel
		if v1json.Unmarshal(raw, &entry) == nil {
			models[id] = entry
		}
	}
	return models
}

func auditFreeCandidate(id string, m publicAuditModel) bool {
	return m.ID == id && model.ValidID(id) && m.Status != "deprecated" &&
		(m.Cost.Text.Input.Price != nil && *m.Cost.Text.Input.Price == 0 &&
			m.Cost.Text.Output.Price != nil && *m.Cost.Text.Output.Price == 0 || strings.HasSuffix(id, "-free"))
}

func openCodeAuditRows(local []openCodeCatalogModel, catalog map[string]publicAuditModel) []model.ModelAudit {
	byID := map[string]model.ModelAudit{}
	for id, entry := range catalog {
		if !auditFreeCandidate(id, entry) {
			continue
		}
		row := newAudit("opencode", "opencode/"+id, openCodeFingerprint, "OpenCode public catalog", entry.Type)
		if len(entry.Name) <= 160 && !model.ContainsSecret([]byte(entry.Name)) && !strings.ContainsAny(entry.Name, "@/") {
			row.Name = entry.Name
		}
		row.Family, row.Context, row.ToolMetadata = entry.Family, entry.Limit.Context, string(entry.Capabilities["tool_call"]) == "true" || string(entry.Capabilities["tools"]) == "true"
		row.Pricing = &model.AuditPrice{Input: entry.Cost.Text.Input.Price, Output: entry.Cost.Text.Output.Price,
			CacheRead: entry.Cost.Text.Input.Cache.Read, CacheWrite: entry.Cost.Text.Input.Cache.Write}
		if row.Pricing.Input != nil && row.Pricing.Output != nil && *row.Pricing.Input == 0 && *row.Pricing.Output == 0 {
			row.Cost = "PUBLIC_ZERO_INPUT_OUTPUT"
		}
		if entry.Type == "decision" {
			row.Inference, row.Reason, row.Coding, row.Review = "UNSUPPORTED_INTERFACE", "NO_ANONYMOUS_DECISION_ADAPTER", "NOT_SUPPORTED", "NOT_SUPPORTED"
		} else {
			row.Inference, row.Reason = "MODEL_UNAVAILABLE", "NOT_EXPOSED_BY_PINNED_ANONYMOUS_CLIENT"
		}
		byID[id] = row
	}
	for id, free := range openCodeFreeModels(local) {
		if !free {
			continue
		}
		row, known := byID[id]
		if !known {
			row = newAudit("opencode", "opencode/"+id, openCodeFingerprint, "installed anonymous catalog", "chat")
			for _, entry := range local {
				if entry.isAnonymousChat() && entry.ID == id {
					p := entry.Cost[0]
					row.Pricing = &model.AuditPrice{Input: p.Input, Output: p.Output, CacheRead: p.Cache.Read, CacheWrite: p.Cache.Write}
				}
			}
		} else {
			row.Source = "installed anonymous catalog and OpenCode public catalog"
		}
		row.Inference, row.Reason = "NOT_RUN", ""
		row.Cost = "VERIFIED_FREE_INSTALLED"
		if known && (row.Pricing.Input == nil || row.Pricing.Output == nil || *row.Pricing.Input != 0 ||
			*row.Pricing.Output != 0 || row.Pricing.CacheRead != nil && *row.Pricing.CacheRead != 0 ||
			row.Pricing.CacheWrite != nil && *row.Pricing.CacheWrite != 0) {
			row.Cost, row.Inference, row.Reason = "UNVERIFIED", "COST_UNVERIFIED", "PUBLIC_PRICE_NOT_ZERO_OR_UNVERIFIED"
		}
		if row.Model == openCodeNativeModel {
			row.Coding, row.Review = "PASS_M2A", "PASS_M2A"
		}
		byID[id] = row
	}
	rows := make([]model.ModelAudit, 0, len(byID))
	for _, row := range byID {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b model.ModelAudit) int { return cmp.Compare(a.Model, b.Model) })
	return rows
}

func AuditOpenCode(ctx context.Context, live bool, cooldown AuditCooldown, previous []model.ModelAudit) ([]model.ModelAudit, error) {
	public, publicErr := fetchOpenCodePublicCatalog(ctx)
	dir, err := os.MkdirTemp("", "ahenk-audit-opencode-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	var rows []model.ModelAudit
	var sharedRetry *time.Time
	for _, prior := range previous {
		if prior.Provider == "opencode" && prior.Binary == openCodeFingerprint && prior.RetryAt != nil &&
			(prior.RateScope == "pool" || prior.RateScope == "account") && time.Now().Before(*prior.RetryAt) &&
			(sharedRetry == nil || prior.RetryAt.After(*sharedRetry)) {
			sharedRetry = prior.RetryAt
		}
	}
	run := func(o openCodeRuntime) error {
		local, err := o.catalog(ctx)
		if err != nil {
			return err
		}
		publicModels := publicAuditModels(public)
		if publicErr == nil && len(publicModels) == 0 {
			publicErr = model.Error("OPENCODE_CATALOG_UNAVAILABLE", "public catalog model metadata could not be decoded")
		}
		rows = openCodeAuditRows(local, publicModels)
		if !live {
			return nil
		}
		for i := range rows {
			row := &rows[i]
			if row.Inference != "NOT_RUN" || reuseAudit(row, previous) {
				continue
			}
			if sharedRetry != nil {
				row.Inference, row.Reason, row.RetryAt = "RATE_LIMITED", "SHARED_LIMIT_RECORDED", sharedRetry
				continue
			}
			if cooldown != nil {
				if at := cooldown(row.Model, time.Now().UTC()); at != nil {
					row.Inference, row.Reason, row.RetryAt = "RATE_LIMITED", "EXISTING_COOLDOWN", at
					continue
				}
			}
			if ctx.Err() != nil {
				row.Reason = "AUDIT_CANCELLED"
				continue
			}
			probeOpenCodeAudit(ctx, o, local, row, &sharedRetry)
		}
		return nil
	}
	if err := checkOpenCodeNativeMachinePolicy(); err != nil {
		return openCodeAuditRows(nil, publicAuditModels(public)), err
	}
	err = withOpenCode(ctx, dir, nil, true, run)
	if err != nil {
		keep = errors.Is(err, execution.ErrCleanup)
		return rows, err
	}
	for i := range rows {
		row := &rows[i]
		if row.Model != openCodeLongCatModel || row.Inference != "MODEL_UNAVAILABLE" {
			continue
		}
		catalog, err := projectOpenCodeLongCatCatalog(public)
		if err != nil {
			row.Cost, row.Inference, row.Reason = "UNVERIFIED", "COST_UNVERIFIED", "CATALOG_PIN_CHANGED"
			break
		}
		row.Cost, row.Inference, row.Reason = "VERIFIED_FREE_PUBLIC_PIN", "NOT_RUN", ""
		if live && reuseAudit(row, previous) && row.Reason != "PINNED_CATALOG_CLIENT_FAILED" {
			break
		}
		if !live {
			break
		}
		if sharedRetry != nil {
			row.Inference, row.Reason, row.RetryAt = "RATE_LIMITED", "SHARED_LIMIT_RECORDED", sharedRetry
			break
		}
		if cooldown != nil {
			if at := cooldown(row.Model, time.Now().UTC()); at != nil {
				row.Inference, row.Reason, row.RetryAt = "RATE_LIMITED", "EXISTING_COOLDOWN", at
				break
			}
		}
		work, err := os.MkdirTemp(dir, "pinned-catalog-")
		if err != nil {
			return rows, err
		}
		err = withOpenCode(ctx, work, catalog, true, func(o openCodeRuntime) error {
			local, err := o.catalog(ctx)
			if err != nil {
				return err
			}
			probeOpenCodeAudit(ctx, o, local, row, &sharedRetry)
			return nil
		})
		if err != nil {
			if errors.Is(err, execution.ErrCleanup) {
				keep = true
				row.Reason = "CLEANUP_UNCONFIRMED"
				return rows, err
			}
			row.Inference, row.Reason = "MODEL_UNAVAILABLE", "PINNED_CATALOG_CLIENT_FAILED"
		}
	}
	return rows, publicErr
}

func probeOpenCodeAudit(ctx context.Context, o openCodeRuntime, local []openCodeCatalogModel, row *model.ModelAudit, sharedRetry **time.Time) {
	id := strings.TrimPrefix(row.Model, "opencode/")
	if !openCodeFreeModels(local)[id] {
		row.Cost, row.Inference, row.Reason = "UNVERIFIED", "COST_UNVERIFIED", "ANONYMOUS_PRICE_GATE_CLOSED"
		return
	}
	if !slices.ContainsFunc(local, func(m openCodeCatalogModel) bool { return nativeRouteMatches(m, id) }) {
		row.Inference, row.Reason = "UNSUPPORTED_INTERFACE", "NATIVE_ROUTE_METADATA_UNVERIFIED"
		return
	}
	start := time.Now()
	out, err := o.runNative(ctx, local, id, "Use no tools. "+auditPrompt)
	if out.OpenCode != nil {
		e := out.OpenCode
		row.Actual, row.Terminal = e.Model, e.TerminalVerified
		if e.PromptSent {
			row.LatencyMS = time.Since(start).Milliseconds()
		}
		row.InputTokens, row.OutputTokens = e.InputTokens, e.OutputTokens
		if e.ReportedCost != nil && *e.ReportedCost == 0 {
			row.RuntimeCost = "ZERO_REPORTED"
		}
	}
	completeAudit(row, out.Summary, err)
	if row.Inference == "RATE_LIMITED" {
		var fault *model.Fault
		if errors.As(err, &fault) {
			at := time.Now().UTC().Add(max(time.Minute, time.Duration(min(fault.RetryAfterMS, int64((1<<63-1)/time.Millisecond)))*time.Millisecond))
			row.RetryAt, row.RateScope = &at, fault.ProviderRateLimitScope
			if fault.ProviderRateLimitScope == "pool" || fault.ProviderRateLimitScope == "account" {
				*sharedRetry = &at
			}
		}
	}
}

func completeAudit(row *model.ModelAudit, summary string, err error) {
	if err != nil {
		var fault *model.Fault
		code := "EXECUTION_ERROR"
		if errors.As(err, &fault) {
			code = fault.Code
			if fault.ProviderReason == "MODEL_ID_MISMATCH" {
				code = fault.ProviderReason
			}
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "ATTEMPT_TIMEOUT"
		}
		row.Reason = code
		switch code {
		case "ZERO_COST_UNVERIFIED", "ZERO_COST_UNAVAILABLE", "BINARY_UNVERIFIED", "BINARY_CHANGED":
			row.Inference = "COST_UNVERIFIED"
		case "RATE_LIMIT":
			row.Inference = "RATE_LIMITED"
		case "QUOTA_EXHAUSTED":
			row.Inference, row.QuotaState = "QUOTA_EXHAUSTED", "EXHAUSTED"
		case "ACCESS_DENIED", "AUTH_REQUIRED", "PERMISSION_DENIED":
			row.Inference = "ACCESS_DENIED"
		case "MODEL_UNAVAILABLE", "MODEL_UNVERIFIED", "OPENCODE_UPSTREAM_HTTP_400":
			row.Inference = "MODEL_UNAVAILABLE"
		case "MODEL_ID_MISMATCH":
			row.Inference = "MODEL_ID_MISMATCH"
		case "AGY_INIT_MISMATCH":
			if row.Actual != "" && row.Actual != row.Model {
				row.Inference = "MODEL_ID_MISMATCH"
			}
		case "ATTEMPT_TIMEOUT":
			row.Inference = "TIMEOUT"
		case "INVALID_OUTPUT":
			if row.Terminal {
				row.Inference = "QUALITY_FAIL"
			}
		}
		return
	}
	if row.Actual != row.Model {
		row.Inference, row.Reason = "MODEL_ID_MISMATCH", "RETURNED_MODEL_DIFFERS"
		return
	}
	var answer struct {
		Value *int `json:"value"`
	}
	if !row.Terminal || json.Unmarshal([]byte(summary), &answer, json.RejectUnknownMembers(true)) != nil || answer.Value == nil || *answer.Value != 6 {
		row.Inference, row.Reason = "QUALITY_FAIL", "EXPECTED_GO_INTEGER_NOT_VERIFIED"
		return
	}
	row.Inference, row.Role, row.Reason = "PASS", "synthetic_read_only", ""
	if row.Coding == "PASS_M2A" && row.Review == "PASS_M2A" {
		row.Role = "trusted_local_coding_and_review"
	}
}
