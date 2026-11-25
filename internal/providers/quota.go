// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	quotaMaxAge     = 15 * time.Minute
	BarCodexBurst   = "codex_5h_burst"
	BarCodexWeekly  = "codex_weekly"
	BarAgyGemini    = "agy_gemini_5h"
	BarAgyOpus      = "agy_opus_5d"
	BarAgyOpus5h    = "agy_opus_5h"
	BarAgyArgon     = "gemini-argon-wtu-weekly"
	BarOpenCodeFree = "opencode_free_16h"
)

type codexBarWindow struct {
	UsedPercent *float64   `json:"usedPercent"`
	UsageKnown  *bool      `json:"usageKnown"`
	ResetsAt    *time.Time `json:"resetsAt"`
	Minutes     int        `json:"windowMinutes"`
	Placeholder bool       `json:"isSyntheticPlaceholder"`
}

type codexBarRecord struct {
	Provider string          `json:"provider"`
	Source   string          `json:"source"`
	Error    json.RawMessage `json:"error"`
	Usage    struct {
		UpdatedAt  time.Time `json:"updatedAt"`
		Confidence string    `json:"dataConfidence"`
		Identity   struct {
			ProviderID string `json:"providerID"`
			Email      string `json:"accountEmail"`
		} `json:"identity"`
		Primary   *codexBarWindow `json:"primary"`
		Secondary *codexBarWindow `json:"secondary"`
		Tertiary  *codexBarWindow `json:"tertiary"`
		Extra     []struct {
			ID         string          `json:"id"`
			Window     *codexBarWindow `json:"window"`
			UsageKnown *bool           `json:"usageKnown"`
		} `json:"extraRateWindows"`
	} `json:"usage"`
}

func (q Quota) At(now time.Time) Quota {
	if q.Remaining != nil && (q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) > quotaMaxAge ||
		q.ResetAt != nil && !q.ResetAt.After(now)) {
		q.State, q.Remaining = "STALE", nil
	}
	return q
}

func agyBucket(id string) (bar, family string, minutes int) {
	switch id {
	case "gemini-5h":
		return BarAgyGemini, "gemini", 300
	case "gemini-weekly":
		return "gemini-weekly", "gemini", 10080
	case "3p-5h":
		return BarAgyOpus5h, "claude_gpt", 300
	case "3p-weekly":
		return BarAgyOpus, "claude_gpt", 7200
	case "gemini-argon-wtu-weekly":
		return BarAgyArgon, "gemini_argon", 10080
	default:
		return id, "", 0
	}
}

// AGYLongWindow returns the canonical long bucket and its duration for a family.
func AGYLongWindow(family string) (string, int) {
	for _, id := range AGYFamilyBuckets(family) {
		bar, _, minutes := agyBucket(id)
		if minutes > 300 {
			return bar, minutes
		}
	}
	return "", 0
}

func (q Quota) IsAGYLongWindow(family string) bool {
	bar, minutes := AGYLongWindow(family)
	name, _, _ := agyBucket(q.Bucket)
	// Older CodexBar observers reported the named 3p window as seven days.
	return bar != "" && q.Family == family && name == bar && (q.WindowMinutes == 0 || q.WindowMinutes == minutes || family == "claude_gpt" && q.WindowMinutes == 10080)
}

func AGYModelFamily(id string) string {
	switch {
	case strings.HasPrefix(id, "gemini-4-argon-") || id == "gemini-4-argon":
		return "gemini_argon"
	case strings.HasPrefix(id, "gemini-3.8-flash-") || id == "gemini-3.8-flash":
		return "gemini"
	case strings.HasPrefix(id, "claude-"), strings.HasPrefix(id, "gpt-"):
		return "claude_gpt"
	default:
		return ""
	}
}

// DecodeCodexBar normalizes only quota windows. It drops account identities,
// credits and raw errors. AGY's named buckets supersede its positional aliases.
func DecodeCodexBar(r io.Reader, requested string, now time.Time) ([]Quota, error) {
	return decodeCodexBar(r, requested, "", "", now)
}

// requestedSource is set only by ReadCodexBar after verifying the CodexBar
// binary against its codesign identity and hash allowlist.
func decodeCodexBar(r io.Reader, requested, requestedSource, expectedAccount string, now time.Time) ([]Quota, error) {
	switch requested {
	case "codex", "antigravity", "copilot", "claude":
	default:
		return nil, errors.New("unsupported quota provider")
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("quota export exceeds read limit")
	}
	data = bytes.TrimSpace(data)
	var records []codexBarRecord
	var target any = &records
	if len(data) > 0 && data[0] == '{' {
		records = make([]codexBarRecord, 1)
		target = &records[0]
	}
	if requested == "claude" {
		// Require exact field names and reject duplicate JSON members.
		err = jsonv2.Unmarshal(data, target)
	} else {
		err = json.Unmarshal(data, target)
	}
	if err != nil {
		return nil, errors.New("invalid CodexBar export")
	}
	id := requested
	if requested == "antigravity" {
		id = "agy"
	}
	if len(records) != 1 || records[0].Provider != requested {
		return []Quota{UnknownQuota(id)}, nil
	}
	row := records[0]
	if row.Usage.Identity.ProviderID != "" && row.Usage.Identity.ProviderID != requested || len(row.Error) > 0 && string(row.Error) != "null" || row.Usage.UpdatedAt.IsZero() || row.Usage.UpdatedAt.After(now) {
		return []Quota{UnknownQuota(id)}, nil
	}
	accountMatch := "UNKNOWN"
	oauthProviders := requested == "codex" || requested == "claude" || requested == "copilot"
	trustedSource := oauthProviders && requestedSource == "oauth" && row.Source == "oauth" ||
		requested == "antigravity" && requestedSource == "cli" && (row.Source == "app" || row.Source == "cli")
	if email := strings.ToLower(strings.TrimSpace(row.Usage.Identity.Email)); trustedSource && expectedAccount != "" &&
		row.Usage.Identity.ProviderID == requested && email != "" {
		accountMatch = "MISMATCH"
		if strings.EqualFold(model.Hash([]byte(email)), expectedAccount) {
			accountMatch = "MATCHED"
		}
	}
	if id == "claude" && (accountMatch != "MATCHED" || row.Usage.Confidence != "exact" ||
		now.Sub(row.Usage.UpdatedAt) > quotaMaxAge || row.Usage.Tertiary != nil || len(row.Usage.Extra) != 0) {
		// The fixture establishes only positional account windows. Additional
		// windows may alias them or carry a narrower scope.
		q := UnknownQuota(id)
		if accountMatch == "MISMATCH" {
			q.AccountMatch = accountMatch
		}
		return []Quota{q}, nil
	}
	var out []Quota
	add := func(bucket, family string, w *codexBarWindow, namedKnown *bool) {
		if w == nil {
			return
		}
		q := UnknownQuota(id)
		q.Bucket, q.Family, q.WindowMinutes = bucket, family, w.Minutes
		q.Source, q.RequestedSource = row.Source, requestedSource
		q.AccountMatch = accountMatch
		// Expose known vendor labels only. Do not retain arbitrary strings from exports.
		switch q.Source {
		case "app", "cli", "ide", "oauth", "web", "offline", "api":
		default:
			q.Source = "unknown"
		}
		if row.Usage.Confidence == "exact" {
			q.Confidence = "exact"
		}
		q.ObservedAt, q.ResetAt = row.Usage.UpdatedAt, w.ResetsAt
		// In the pinned schema, NamedRateWindow omits usageKnown only when true.
		known := requestedSource != "" || namedKnown != nil || w.UsageKnown != nil ||
			row.Source == "cli" || (requested == "codex" && row.Source == "oauth")
		known = known && (namedKnown == nil || *namedKnown) && (w.UsageKnown == nil || *w.UsageKnown)
		if id == "claude" {
			minutes := 300
			if bucket == "secondary" {
				minutes = 10080
			}
			known = known && w.Minutes == minutes && w.ResetsAt != nil && w.ResetsAt.After(now) &&
				!w.ResetsAt.After(q.ObservedAt.Add(time.Duration(minutes)*time.Minute))
		}
		if known && !w.Placeholder && q.Source != "offline" && w.UsedPercent != nil && *w.UsedPercent >= 0 && *w.UsedPercent <= 100 {
			left := 100 - *w.UsedPercent
			if rounded := math.Round(left*10) / 10; rounded > 0 {
				left = rounded
			}
			q.Unit, q.State, q.Remaining = "percent", "KNOWN", &left
			if left == 0 {
				q.State = "EXHAUSTED"
			}
		}
		out = append(out, q.At(now))
	}
	if id == "agy" {
		const prefix = "antigravity-quota-summary-"
		seen := map[string]bool{}
		for _, extra := range row.Usage.Extra {
			bucket := strings.TrimPrefix(extra.ID, prefix)
			if !model.ValidID(bucket) || extra.Window == nil {
				continue
			}
			if seen[bucket] {
				return []Quota{UnknownQuota(id)}, nil
			}
			seen[bucket] = true
			bar, family, minutes := agyBucket(bucket)
			if minutes > 0 && extra.Window.Minutes > 0 && minutes != extra.Window.Minutes &&
				!(Quota{Bucket: bar, Family: family, WindowMinutes: extra.Window.Minutes}).IsAGYLongWindow(family) {
				family = ""
			}
			add(bar, family, extra.Window, extra.UsageKnown)
		}
	}
	if id == "codex" {
		add(BarCodexBurst, "", row.Usage.Primary, nil)
		add(BarCodexWeekly, "", row.Usage.Secondary, nil)
		add("tertiary", "", row.Usage.Tertiary, nil)
	} else if len(out) == 0 {
		add("primary", "", row.Usage.Primary, nil)
		add("secondary", "", row.Usage.Secondary, nil)
		add("tertiary", "", row.Usage.Tertiary, nil)
	}
	if id == "codex" {
		seen := map[string]bool{}
		for _, extra := range row.Usage.Extra {
			if !model.ValidID(extra.ID) || extra.Window == nil || seen[extra.ID] || len(out) >= 32 {
				continue
			}
			seen[extra.ID] = true
			add("extra:"+extra.ID, "", extra.Window, extra.UsageKnown)
		}
	}
	if len(out) == 0 {
		return []Quota{UnknownQuota(id)}, nil
	}
	return out, nil
}

// FPTier reports the price/performance tier of a route: "free" (OpenCode
// anonymous), "cheap" (AGY Gemini Flash), "balanced" (Sonnet-class) or
// "hard" (Argon, Codex and Claude frontier).
func FPTier(provider, modelID string) string {
	switch provider {
	case "opencode":
		return "free"
	case "codex", "claude":
		return "hard"
	case "copilot":
		return "cheap"
	case "agy":
		switch AGYModelFamily(modelID) {
		case "gemini_argon":
			return "hard"
		case "claude_gpt":
			return "balanced"
		case "gemini":
			return "cheap"
		}
		return ""
	default:
		return ""
	}
}
