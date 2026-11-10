// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const quotaMaxAge = 15 * time.Minute

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

func agyBucket(id string) (family string, minutes int) {
	switch id {
	case "gemini-5h":
		return "gemini", 300
	case "gemini-weekly":
		return "gemini", 10080
	case "3p-5h":
		return "claude_gpt", 300
	case "3p-weekly":
		return "claude_gpt", 10080
	case "gemini-argon-wtu-weekly":
		return "gemini_argon", 10080
	default:
		return "", 0
	}
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

// requestedSource is set only by ReadCodexBar after validating the pinned 0.83.0 binary.
func decodeCodexBar(r io.Reader, requested, requestedSource, expectedAccount string, now time.Time) ([]Quota, error) {
	switch requested {
	case "codex", "antigravity", "copilot":
	default:
		return nil, errors.New("unsupported quota provider")
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("quota export exceeds read limit")
	}
	data = bytes.TrimSpace(data)
	var records []codexBarRecord
	if len(data) > 0 && data[0] == '{' {
		records = make([]codexBarRecord, 1)
		err = json.Unmarshal(data, &records[0])
	} else {
		err = json.Unmarshal(data, &records)
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
	trustedSource := requested == "codex" && requestedSource == "oauth" && row.Source == "oauth" ||
		requested == "antigravity" && requestedSource == "cli" && (row.Source == "app" || row.Source == "cli")
	if email := strings.ToLower(strings.TrimSpace(row.Usage.Identity.Email)); trustedSource && expectedAccount != "" &&
		row.Usage.Identity.ProviderID == requested && email != "" {
		accountMatch = "MISMATCH"
		if strings.EqualFold(model.Hash([]byte(email)), expectedAccount) {
			accountMatch = "MATCHED"
		}
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
		known := requestedSource != "" || namedKnown != nil || w.UsageKnown != nil
		known = known && (namedKnown == nil || *namedKnown) && (w.UsageKnown == nil || *w.UsageKnown)
		if known && !w.Placeholder && q.Source != "offline" && w.UsedPercent != nil && *w.UsedPercent >= 0 && *w.UsedPercent <= 100 {
			left := 100 - *w.UsedPercent
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
			bucket, ok := strings.CutPrefix(extra.ID, prefix)
			if !ok || !model.ValidID(bucket) || extra.Window == nil {
				continue
			}
			if seen[bucket] {
				return []Quota{UnknownQuota(id)}, nil
			}
			seen[bucket] = true
			family, minutes := agyBucket(bucket)
			if minutes != extra.Window.Minutes {
				family = ""
			}
			add(bucket, family, extra.Window, extra.UsageKnown)
		}
	}
	if id == "codex" || len(out) == 0 {
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
