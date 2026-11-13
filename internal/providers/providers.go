// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

type Registry struct {
	Candidates []model.Capability
	agy        model.ProviderConfig
	codex      model.ProviderConfig
	claude     model.ProviderConfig
}

var defaultCodexModels = []string{
	"gpt-6.1-sol",
	"gpt-6.1-terra",
	"gpt-6-astra",
	"gpt-6-luna",
}

var defaultClaudeModels = []string{
	"claude-opus-5-5",
	"claude-sonnet-5-5",
}

func New(c model.Config) Registry {
	r := Registry{Candidates: []model.Capability{
		{Provider: "agy", Status: "unverified", Reason: "ZERO_COST_UNVERIFIED", Mode: "headless",
			Cost: agyUnverifiedCost(), Profiles: agyProfiles()},
		{Provider: "opencode", Status: "unverified", Reason: "PERMISSION_AND_FREE_MODEL_SMOKE_REQUIRED", Mode: "tool_free"},
		{Provider: "copilot", Status: "disabled", Reason: "CLAUDE_ONLY_UNAVAILABLE"},
	}}
	for _, m := range defaultCodexModels {
		r.Candidates = append(r.Candidates, model.Capability{
			Provider: "codex",
			Model:    m,
			Status:   "disabled",
			Reason:   "BINARY_UNAVAILABLE",
			Mode:     "cli",
			Speed:    "fast",
		})
	}
	for _, m := range defaultClaudeModels {
		r.Candidates = append(r.Candidates, model.Capability{
			Provider: "claude",
			Model:    m,
			Status:   "disabled",
			Reason:   "BINARY_UNAVAILABLE",
			Mode:     "cli",
			Speed:    "fast",
		})
	}
	for _, p := range c.Providers {
		i := slices.IndexFunc(r.Candidates, func(m model.Capability) bool { return m.Provider == p.ID })
		if i < 0 || p.ID == "copilot" {
			continue
		}
		if p.ID == "codex" {
			r.codex = p
			canonicalPath := p.Path
			if canonicalPath == "" {
				if lp, err := exec.LookPath("codex"); err == nil {
					canonicalPath = lp
				}
			}
			if canonicalPath != "" {
				if abs, err := filepath.Abs(canonicalPath); err == nil {
					if real, err := filepath.EvalSymlinks(abs); err == nil && filepath.IsAbs(real) {
						canonicalPath = real
					}
				}
			}
			fp, err := Fingerprint(canonicalPath)
			if err == nil {
				r.codex.Path = canonicalPath
				for idx := range r.Candidates {
					if r.Candidates[idx].Provider == "codex" {
						r.Candidates[idx].Status = "available"
						r.Candidates[idx].Reason = "CODEX_CLI_AVAILABLE"
						r.Candidates[idx].Fingerprint = fp
						if p.Speed != "" {
							r.Candidates[idx].Speed = p.Speed
						}
					}
				}
				if p.Model != "" && !slices.Contains(defaultCodexModels, p.Model) {
					if match := slices.IndexFunc(r.Candidates, func(other model.Capability) bool {
						return other.Provider == "codex" && other.Model == p.Model
					}); match >= 0 {
						r.Candidates[match].Status = "available"
						r.Candidates[match].Reason = "CODEX_CLI_AVAILABLE"
						r.Candidates[match].Fingerprint = fp
						if p.Speed != "" {
							r.Candidates[match].Speed = p.Speed
						}
					} else {
						r.Candidates = append(r.Candidates, model.Capability{
							Provider:    "codex",
							Model:       p.Model,
							Status:      "available",
							Reason:      "CODEX_CLI_AVAILABLE",
							Fingerprint: fp,
							Mode:        "cli",
							Speed:       p.Speed,
						})
					}
				}
			} else {
				for idx := range r.Candidates {
					if r.Candidates[idx].Provider == "codex" {
						r.Candidates[idx].Status = "disabled"
						r.Candidates[idx].Reason = "BINARY_UNAVAILABLE"
					}
				}
			}
			continue
		}
		if p.ID == "claude" {
			r.claude = p
			canonicalPath := p.Path
			if canonicalPath == "" {
				if lp, err := exec.LookPath("claude"); err == nil {
					canonicalPath = lp
				}
			}
			if canonicalPath != "" {
				if abs, err := filepath.Abs(canonicalPath); err == nil {
					if real, err := filepath.EvalSymlinks(abs); err == nil && filepath.IsAbs(real) {
						canonicalPath = real
					}
				}
			}
			fp, err := Fingerprint(canonicalPath)
			if err == nil {
				r.claude.Path = canonicalPath
				for idx := range r.Candidates {
					if r.Candidates[idx].Provider == "claude" {
						r.Candidates[idx].Status = "available"
						r.Candidates[idx].Reason = "CLAUDE_CLI_AVAILABLE"
						r.Candidates[idx].Fingerprint = fp
						if p.Speed != "" {
							r.Candidates[idx].Speed = p.Speed
						}
					}
				}
				if p.Model != "" && !slices.Contains(defaultClaudeModels, p.Model) {
					if match := slices.IndexFunc(r.Candidates, func(other model.Capability) bool {
						return other.Provider == "claude" && other.Model == p.Model
					}); match >= 0 {
						r.Candidates[match].Status = "available"
						r.Candidates[match].Reason = "CLAUDE_CLI_AVAILABLE"
						r.Candidates[match].Fingerprint = fp
						if p.Speed != "" {
							r.Candidates[match].Speed = p.Speed
						}
					} else {
						r.Candidates = append(r.Candidates, model.Capability{
							Provider:    "claude",
							Model:       p.Model,
							Status:      "available",
							Reason:      "CLAUDE_CLI_AVAILABLE",
							Fingerprint: fp,
							Mode:        "cli",
							Speed:       p.Speed,
						})
					}
				}
			} else {
				for idx := range r.Candidates {
					if r.Candidates[idx].Provider == "claude" {
						r.Candidates[idx].Status = "disabled"
						r.Candidates[idx].Reason = "BINARY_UNAVAILABLE"
					}
				}
			}
			continue
		}
		if p.ID == "opencode" {
			candidate := openCodeCapability(p)
			match := slices.IndexFunc(r.Candidates, func(other model.Capability) bool {
				return other.Provider == candidate.Provider && other.Model == candidate.Model &&
					model.OpenCodeTransportID(other.OpenCodeTransport) == model.OpenCodeTransportID(candidate.OpenCodeTransport)
			})
			switch {
			case match >= 0:
				if r.Candidates[match].Status != "available" {
					r.Candidates[match] = candidate
				}
			case r.Candidates[i].Model == "":
				r.Candidates[i] = candidate
			default:
				r.Candidates = append(r.Candidates, candidate)
			}
			continue
		}
		m := &r.Candidates[i]
		m.Model = p.Model
		m.Speed = p.Speed
		m.AttestedAccount = p.AttestedAccount
		m.AttestedAccountSHA256 = p.AttestedAccountSHA256
		if p.Path == "" {
			continue
		}
		fp, err := Fingerprint(p.Path)
		if err != nil {
			m.Reason = "BINARY_UNAVAILABLE"
			continue
		}
		m.Fingerprint = fp
		if p.ExpectedBinarySHA256 != "" && p.ExpectedBinarySHA256 != fp {
			m.Reason = "BINARY_CHANGED"
			continue
		}
		if p.ID == "agy" && p.ExpectedBinarySHA256 == fp && fp == agy154Fingerprint {
			m.Reason = "AGY_EXECUTION_BINARY_UNVERIFIED"
		}
		if p.ID == "agy" && p.EnableSynthetic && AGYWorkerIdentityMatches(p.Model, fp) &&
			p.ExpectedBinarySHA256 == fp {
			r.agy = p
			m.Status = "available"
			m.Reason = "AGY_SYNTHETIC_PREFLIGHT_REQUIRED"
			m.Profiles[0].Status, m.Profiles[0].Reason = "preflight_required", "AGY_AGENT_AND_PERMISSION_CHECK_AT_DISPATCH"
			profiles := []int{1}
			m.Profiles[4].Status = "preflight_required"
			if AGYIdentityMatches(p.Model, fp) {
				m.Profiles[4].Status = "preflight_required"
				profiles = append(profiles, 2)
			}
			for _, i := range profiles {
				m.Profiles[i].Status, m.Profiles[i].Reason = "preflight_required", "SCOPED_SNAPSHOT_ONLY"
			}
		}
	}
	if r.codex.Path == "" {
		hasExplicitCodex := slices.ContainsFunc(c.Providers, func(p model.ProviderConfig) bool { return p.ID == "codex" })
		if !hasExplicitCodex {
			if lp, err := exec.LookPath("codex"); err == nil {
				canonicalPath := lp
				if abs, err := filepath.Abs(canonicalPath); err == nil {
					if real, err := filepath.EvalSymlinks(abs); err == nil && filepath.IsAbs(real) {
						canonicalPath = real
					}
				}
				if fp, err := Fingerprint(canonicalPath); err == nil {
					r.codex = model.ProviderConfig{ID: "codex", Path: canonicalPath}
					for idx := range r.Candidates {
						if r.Candidates[idx].Provider == "codex" {
							r.Candidates[idx].Status = "available"
							r.Candidates[idx].Reason = "CODEX_CLI_AVAILABLE"
							r.Candidates[idx].Fingerprint = fp
						}
					}
				}
			}
		}
	}
	if r.claude.Path == "" {
		hasExplicitClaude := slices.ContainsFunc(c.Providers, func(p model.ProviderConfig) bool { return p.ID == "claude" })
		if !hasExplicitClaude {
			if lp, err := exec.LookPath("claude"); err == nil {
				canonicalPath := lp
				if abs, err := filepath.Abs(canonicalPath); err == nil {
					if real, err := filepath.EvalSymlinks(abs); err == nil && filepath.IsAbs(real) {
						canonicalPath = real
					}
				}
				if fp, err := Fingerprint(canonicalPath); err == nil {
					r.claude = model.ProviderConfig{ID: "claude", Path: canonicalPath}
					for idx := range r.Candidates {
						if r.Candidates[idx].Provider == "claude" {
							r.Candidates[idx].Status = "available"
							r.Candidates[idx].Reason = "CLAUDE_CLI_AVAILABLE"
							r.Candidates[idx].Fingerprint = fp
						}
					}
				}
			}
		}
	}
	r.addAuditedModels(c.ModelAudit, time.Now().UTC())
	for i := range r.Candidates {
		c := &r.Candidates[i]
		if c.Status != "available" {
			continue
		}
		c.Quality = []model.QualityEvidence{{Role: "inference", Tier: 1, Source: "SCOPED_ROUTE_PREFLIGHT"}}
		if c.AuditValidUntil != nil {
			c.Quality[0].Source = "LIVE_SYNTHETIC_INFERENCE"
		}
		if c.Provider == "agy" && c.Model == agySyntheticModel && c.Fingerprint == agy154Fingerprint {
			c.Quality = append(c.Quality, model.QualityEvidence{Role: "implementation", Tier: 2, Source: "LIVE_G017_DETERMINISTIC_CHECK", EvidenceSHA256: "385580c7659f00165d713a731e8c57133880700f68088713d10457d0faeb5080"})
			for _, role := range []string{"architecture", "planning", "reasoning", "research"} {
				c.Quality = append(c.Quality, model.QualityEvidence{Role: role, Tier: 1, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION",
					EvidenceSHA256: "49b14ff7654bbec0f6df36bfc577b3193b0c109db7baaa29c50632a1af142cab"})
			}
		}
		if c.Provider == "agy" && AGYIdentityMatches(c.Model, c.Fingerprint) {
			c.Quality = append(c.Quality, model.QualityEvidence{Role: "implementation", Tier: 2, Source: "LIVE_M2A_DETERMINISTIC_CHECK"},
				model.QualityEvidence{Role: "review", Tier: 2, Source: "LIVE_M2A_INDEPENDENT_REVIEW"})
		}
		if c.Provider == "opencode" {
			c.OpenCodeEndpoint = model.OpenCodeFreeEndpoint
			c.Cost = &model.CostEvidence{Policy: "FREE_ONLY", Status: "CATALOG_PREFLIGHT_REQUIRED", Account: model.OpenCodeAnonymous}
			if c.AuditValidUntil == nil && c.Model == openCodeNativeModel && OpenCodeIdentityMatches(c.Model, c.Fingerprint) {
				c.Quality = append(c.Quality, model.QualityEvidence{Role: "review", Tier: 2, Source: "LIVE_M2A_INDEPENDENT_REVIEW"})
				if c.OpenCodeTransport == model.OpenCodeNativeReadAsk {
					c.Quality = append(c.Quality, model.QualityEvidence{Role: "implementation", Tier: 2, Source: "LIVE_M2A_DETERMINISTIC_CHECK"})
				}
			}
		}
		if c.Provider == "codex" || c.Provider == "claude" {
			for _, role := range []string{"implementation", "review", "planning", "architecture", "reasoning", "research"} {
				c.Quality = append(c.Quality, model.QualityEvidence{Role: role, Tier: 2, Source: "CLI_RUNNER"})
			}
		}
		if c.Provider == "agy" && c.Cost == nil {
			c.Cost = agyUnverifiedCost()
		}
	}
	if c.EnableFixture {
		selfFP := execution.SelfBinarySHA256()
		for _, p := range []string{"fixture-a", "fixture-b", "fixture-c"} {
			candidate := model.Capability{Provider: p, Model: "synthetic", Status: "available", Mode: "fixture", Speed: "fixture", Fingerprint: selfFP}
			for _, cfgP := range c.Providers {
				if cfgP.ID == p {
					candidate.AttestedAccount = cfgP.AttestedAccount
					candidate.AttestedAccountSHA256 = cfgP.AttestedAccountSHA256
					break
				}
			}
			r.Candidates = append(r.Candidates, candidate)
		}
	}
	return r
}

func openCodeCapability(p model.ProviderConfig) model.Capability {
	c := model.Capability{Provider: "opencode", Model: p.Model, OpenCodeAccount: model.OpenCodeAnonymous, Speed: p.Speed, Status: "unverified",
		OpenCodeTransport: p.OpenCodeTransport, Reason: "PERMISSION_AND_FREE_MODEL_SMOKE_REQUIRED", Mode: "tool_free",
		AttestedAccount: p.AttestedAccount, AttestedAccountSHA256: p.AttestedAccountSHA256}
	if p.AttestedAccount != "" {
		c.OpenCodeAccount = p.AttestedAccount
	}
	if !openCodeTransportMatches(c.Model, c.OpenCodeTransport) {
		c.Reason = "OPENCODE_TRANSPORT_UNVERIFIED"
		return c
	}
	if p.Path == "" {
		return c
	}
	fp, err := Fingerprint(p.Path)
	if err != nil {
		c.Reason = "BINARY_UNAVAILABLE"
		return c
	}
	c.Fingerprint = fp
	if p.ExpectedBinarySHA256 != "" && p.ExpectedBinarySHA256 != fp {
		c.Reason = "BINARY_CHANGED"
		return c
	}
	if p.EnableSynthetic && p.Path == openCodePath && OpenCodeIdentityMatches(p.Model, fp) && p.ExpectedBinarySHA256 == fp {
		c.Status = "available"
		c.Reason = "SYNTHETIC_READ_ONLY_PRICE_CHECK_AT_DISPATCH"
		if c.Model == openCodeLongCatModel {
			c.OpenCodeCatalogSHA256 = openCodeLongCatCatalogSHA256
		}
	}
	return c
}

// Fingerprint is an L0 observation. It grants no execution capability.
func Fingerprint(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) || real != filepath.Clean(path) {
		return "", errors.New("binary path must be canonical")
	}
	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Size() > 512<<20 {
		return "", errors.New("not a bounded executable file")
	}
	h, err := hashReader(f)
	if err != nil {
		return "", err
	}
	after, err := os.Stat(real)
	if err != nil || !os.SameFile(st, after) || st.ModTime() != after.ModTime() || st.Size() != after.Size() {
		return "", errors.New("binary changed during inspection")
	}
	return h, nil
}

func Estimate(c model.Capability, t model.Task) time.Duration {
	var secs int
	switch {
	case c.Speed == "fixture":
		secs = 5
		if t.Objective == "fixture:slow" || slices.ContainsFunc(t.Acceptance, func(ch model.Check) bool { return ch.CheckID != "" }) {
			secs = 25
		}
	case t.Risk == "critical":
		secs = 480
	case t.Priority == "background" && c.Speed == "slow":
		secs = 420
	case t.Priority == "background":
		secs = 240
	case c.Speed == "slow":
		secs = 240
	case c.Speed == "fast" && t.Mode == "read_only":
		secs = 60
	case c.Speed == "fast":
		secs = 90
	default:
		secs = 180
	}
	return time.Duration(secs) * time.Second
}

func Budget(now, deadline time.Time, t model.Task, estimate time.Duration) (time.Duration, bool) {
	reserve := 20 * time.Second
	if t.Risk == "critical" {
		reserve = 65 * time.Second
	}
	left := deadline.Sub(now) - reserve
	if left < estimate {
		return 0, false
	}
	return min(left, estimate+estimate/4), true
}

func AllowsTrustedLocal(c model.Capability) bool {
	if c.Status != "available" {
		return false
	}
	switch c.Provider {
	case "codex", "claude":
		return true
	case "agy":
		return c.AuditValidUntil == nil && AGYWorkerIdentityMatches(c.Model, c.Fingerprint)
	case "opencode":
		return c.OpenCodeTransport == model.OpenCodeNativeReadAsk && c.Model == openCodeNativeModel &&
			OpenCodeIdentityMatches(c.Model, c.Fingerprint)
	default:
		return false
	}
}

func (r Registry) RunTrustedLocal(ctx context.Context, dir, work string, c model.Capability, t model.Task, writePaths []string) (execution.FixtureInput, error) {
	if !AllowsTrustedLocal(c) || t.ExecutionProfile != model.TrustedLocalProfile || t.Mode != "patch" ||
		t.DataClass != "synthetic" || len(writePaths) == 0 {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "invalid trusted-local provider or task")
	}
	switch c.Provider {
	case "codex":
		modelID := c.Model
		if modelID == "" {
			modelID = r.codex.Model
		}
		effort := t.TargetEffort
		if effort == "" {
			effort = "medium"
		}
		res, err := RunCodexCLI(ctx, work, modelID, effort, t.Objective, SandboxModeWorkspaceWrite, r.codex.Path)
		if err != nil {
			return execution.FixtureInput{}, err
		}
		return execution.FixtureInput{Codex: &res.Evidence, Summary: res.Output}, nil
	case "claude":
		modelID := c.Model
		if modelID == "" {
			modelID = r.claude.Model
		}
		res, err := RunClaudeCLI(ctx, work, modelID, t.Objective, SandboxModeWorkspaceWrite, r.claude.Path)
		if err != nil {
			return execution.FixtureInput{}, err
		}
		return execution.FixtureInput{Claude: &res.Evidence, Summary: res.Output}, nil
	case "agy":
		return superviseAGY(ctx, dir, agyRequest{Config: r.agy, Task: t, Worktree: work, WritePaths: writePaths})
	default:
		return runOpenCodeTrusted(ctx, dir, work, c, t, writePaths)
	}
}

func AllowsNativeChild(c model.Capability, t model.Task) bool {
	return !t.NativeChild || !t.NativeChildBackground && c.Provider == "agy" && AGYIdentityMatches(c.Model, c.Fingerprint) ||
		c.Provider == "opencode" && c.OpenCodeTransport == model.OpenCodeNativeReadAsk && OpenCodeIdentityMatches(c.Model, c.Fingerprint)
}

func AllowsContinuation(c model.Capability, t model.Task) bool {
	return t.FollowUp == "" || c.Provider == "agy" || c.Provider == "opencode" && c.OpenCodeTransport == model.OpenCodeNativeReadAsk
}

// RunPrivate accepts only the exact audited routes approved by the local operator.
func (r Registry) RunPrivate(ctx context.Context, dir string, c model.Capability, t model.Task, files map[string]string, handoffs []model.Handoff, grant *model.PrivateSourcePolicy) (execution.FixtureInput, error) {
	if c.Status != "available" {
		return execution.FixtureInput{}, model.GateError("PRIVATE_SOURCE_ROUTE_DENIED", "PROVIDER_UNAVAILABLE", "provider is not available for private source execution", "verify_provider_availability")
	}
	if ok, gate := grant.RouteDecision(c, t, time.Now()); !ok {
		return execution.FixtureInput{}, model.GateError("PRIVATE_SOURCE_ROUTE_DENIED", gate, "no active private source grant for this route", "verify_private_source_route_and_account_attestation")
	}
	if fault := grant.CheckRequest(t, time.Now()); fault != nil {
		return execution.FixtureInput{}, fault
	}
	if fault := grant.CheckContext(t.AllowedPaths, files); fault != nil {
		return execution.FixtureInput{}, fault
	}
	ctx, cancel := context.WithDeadline(ctx, grant.ExpiresAt)
	defer cancel()
	if c.Mode == "fixture" {
		return r.runFixture(ctx, dir, c, t, files, handoffs)
	}
	if !OpenCodeIdentityMatches(c.Model, c.Fingerprint) || !openCodeTransportMatches(c.Model, c.OpenCodeTransport) {
		return execution.FixtureInput{}, model.GateError("PRIVATE_SOURCE_ROUTE_DENIED", "BINARY_CHANGED", "OpenCode private route did not match the approved worker", "reverify_opencode_binary_sha256")
	}
	return runOpenCode(ctx, dir, c, t, files, handoffs, grant)
}

func (r Registry) Run(ctx context.Context, dir string, c model.Capability, t model.Task, contextFiles map[string]string, handoffs []model.Handoff) (execution.FixtureInput, error) {
	if t.DataClass == model.PrivateSourceData {
		return execution.FixtureInput{}, model.Error("PRIVATE_SOURCE_ROUTE_DENIED", "private source requires an operator grant")
	}
	if !AllowsNativeChild(c, t) || !AllowsContinuation(c, t) || (t.FollowUp != "" || t.NativeChild) && len(handoffs) != 0 {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "continuation requires an owned native read-only session")
	}
	if t.ExecutionProfile == model.TrustedLocalProfile {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "trusted-local requires a private worktree")
	}
	if c.AuditValidUntil != nil && (!AuditRouteAllows(c, t, time.Now().UTC()) || len(handoffs) != 0) {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "model audit admits synthetic read-only work only")
	}
	if len(handoffs) != 0 && !AllowsHandoff(c, t) {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "provider is not approved for this handoff")
	}
	if c.Provider == "codex" && c.Status == "available" {
		sb := SandboxModeWorkspaceWrite
		if t.Mode == "read_only" || t.Mode == "analysis" || t.Mode == "review" {
			sb = SandboxModeReadOnly
		}
		modelID := c.Model
		if modelID == "" {
			modelID = r.codex.Model
		}
		effort := t.TargetEffort
		if effort == "" {
			effort = "medium"
		}
		res, err := RunCodexCLI(ctx, dir, modelID, effort, t.Objective, sb, r.codex.Path)
		if err != nil {
			return execution.FixtureInput{}, err
		}
		return execution.FixtureInput{Codex: &res.Evidence, Summary: res.Output}, nil
	}
	if c.Provider == "claude" && c.Status == "available" {
		sb := SandboxModeWorkspaceWrite
		if t.Mode == "read_only" || t.Mode == "analysis" || t.Mode == "review" {
			sb = SandboxModeReadOnly
		}
		modelID := c.Model
		if modelID == "" {
			modelID = r.claude.Model
		}
		res, err := RunClaudeCLI(ctx, dir, modelID, t.Objective, sb, r.claude.Path)
		if err != nil {
			return execution.FixtureInput{}, err
		}
		return execution.FixtureInput{Claude: &res.Evidence, Summary: res.Output}, nil
	}
	if c.Provider == "agy" {
		if c.Status != "available" || !r.agy.EnableSynthetic {
			return execution.FixtureInput{}, model.Error("ZERO_COST_UNVERIFIED", "AGY synthetic preflight is unavailable")
		}
		if !AllowsAGYProfile(c, t) {
			return execution.FixtureInput{}, model.Error("AGY_PROFILE_UNVERIFIED", "AGY task requires an unverified profile")
		}
		if c.AuditValidUntil != nil {
			cfg := r.agy
			cfg.Model = c.Model
			return superviseAGY(ctx, dir, agyRequest{Config: cfg, Audited: true, Task: t, Files: contextFiles})
		}
		return runAGY(ctx, dir, r.agy, t, contextFiles, handoffs)
	}
	if c.Provider == "opencode" && c.Status == "available" && c.Mode == "tool_free" &&
		(c.OpenCodeAccount == "" || c.OpenCodeAccount == model.OpenCodeAnonymous) && OpenCodeIdentityMatches(c.Model, c.Fingerprint) &&
		(openCodeTransportMatches(c.Model, c.OpenCodeTransport) || c.AuditValidUntil != nil && c.OpenCodeTransport == model.OpenCodeNativeReadAsk) {
		return runOpenCode(ctx, dir, c, t, contextFiles, handoffs, nil)
	}
	if c.Mode != "fixture" || c.Status != "available" {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "live execution requires verified permission and cost evidence")
	}
	return r.runFixture(ctx, dir, c, t, contextFiles, handoffs)
}

func (r Registry) runFixture(ctx context.Context, dir string, c model.Capability, t model.Task, contextFiles map[string]string, handoffs []model.Handoff) (execution.FixtureInput, error) {
	in := execution.FixtureInput{Summary: "Synthetic adapter completed the offline fixture."}
	// The fixture is explicitly enabled and never interprets the objective as a command.
	if t.Objective == "fixture:slow" {
		in.DelayMillis = 25000
	}
	if t.Objective == "fixture:429" && c.Provider == "fixture-a" {
		in.Failure = "RATE_LIMIT"
	}
	if t.Objective == "fixture:handoff" {
		if len(handoffs) == 1 {
			in.Summary = "Received verified " + handoffs[0].Manifest.Kind + " " + model.Hash([]byte(handoffs[0].Content))
		} else {
			in.Failure = "INVALID_OUTPUT"
		}
	}
	if len(handoffs) == 1 && handoffs[0].Review != nil {
		in.Summary = `{"verdict":"APPROVED","findings":[]}`
		if t.Objective == "fixture:review-reject" {
			in.Summary = `{"verdict":"CHANGES_REQUESTED","findings":[{"severity":"medium","message":"The fixture replaces the expected original value."}]}`
		}
	}
	if t.Mode == "patch" {
		for _, name := range t.AllowedPaths {
			body, ok := contextFiles[name]
			if !ok || strings.ContainsAny(name, " \"") {
				continue
			}
			if body == "before\n" {
				in.Patch = fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-before\n+after\n", name, name, name, name)
				break
			}
			if strings.Contains(body, `"before"`) {
				oldLines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
				newBody := strings.Replace(body, `"before"`, `"after"`, 1)
				newLines := strings.Split(strings.TrimSuffix(newBody, "\n"), "\n")
				var b strings.Builder
				fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", name, name, name, name, len(oldLines), len(newLines))
				for _, l := range oldLines {
					b.WriteString("-" + l + "\n")
				}
				for _, l := range newLines {
					b.WriteString("+" + l + "\n")
				}
				in.Patch = b.String()
				break
			}
		}
	}
	return execution.RunFixture(ctx, dir, in)
}

type Quota struct {
	RetryAt           *time.Time              `json:"retry_at,omitempty"`
	RecoveryState     string                  `json:"recovery_state,omitempty"`
	CooldownScope     string                  `json:"cooldown_scope,omitempty"`
	Endpoint          string                  `json:"endpoint,omitempty"`
	Quality           []model.QualityEvidence `json:"quality_evidence,omitempty"`
	Cost              *model.CostEvidence     `json:"cost,omitempty"`
	LastSuccess       *time.Time              `json:"last_success_at,omitempty"`
	LastFailure       *time.Time              `json:"last_failure_at,omitempty"`
	LastFailureCode   string                  `json:"last_failure_code,omitempty"`
	RetrySource       string                  `json:"retry_source,omitempty"`
	OpenCodeAccount   string                  `json:"opencode_account,omitempty"`
	OpenCodeTransport string                  `json:"opencode_transport,omitempty"`
	OpenCodeProvider  string                  `json:"opencode_provider,omitempty"`
	Provider          string                  `json:"provider"`
	Model             string                  `json:"model,omitempty"`
	HealthScope       string                  `json:"health_scope,omitempty"`
	Bucket            string                  `json:"bucket"`
	Family            string                  `json:"family,omitempty"`
	WindowMinutes     int                     `json:"window_minutes,omitempty"`
	Reason            string                  `json:"reason,omitempty"`
	Unit              string                  `json:"unit"`
	Remaining         *float64                `json:"remaining"`
	State             string                  `json:"state"`
	Source            string                  `json:"source,omitempty"`
	RequestedSource   string                  `json:"requested_source,omitempty"`
	Confidence        string                  `json:"confidence,omitempty"`
	AccountMatch      string                  `json:"account_match"`
	ObservedAt        time.Time               `json:"observed_at,omitzero"`
	ResetAt           *time.Time              `json:"reset_at,omitempty"`
}

func UnknownQuota(provider string) Quota {
	return Quota{Provider: provider, Bucket: "unknown", Unit: "unknown", State: "UNKNOWN", AccountMatch: "UNKNOWN"}
}

func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
