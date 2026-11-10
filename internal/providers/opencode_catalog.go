// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// The bundled catalog in OpenCode 2.2.6 predates this anonymous model.
// Pin the reviewed public entry, including its native route and pricing.
const (
	openCodeLongCatModel         = "opencode/longcat-2.5-free"
	openCodeLongCatCatalogSHA256 = "bb796b3398e2f13ed32115460f17d9f28f5124f7a552a4faf7e919e38e1b64e5"
	openCodeLongCatModelID       = "longcat-2.5-free"
	openCodePublicCatalog        = "https://models.opencode.ai/experimental/v2.0/api.json?type=chat,decision,image"
	openCodePublicCatalogLimit   = 16_000_000
)

// This is the official client's public catalog GET. Pin the exact projection
// before letting OpenCode load it, as the client normalizes absent prices to
// zero. The reviewed original contains explicit zero input, output and cache
// read prices. Console does not price cache writes for this model.
func loadOpenCodeLongCatCatalog(ctx context.Context) ([]byte, error) {
	body, err := fetchOpenCodePublicCatalog(ctx)
	if err != nil {
		return nil, longCatCatalogError()
	}
	return projectOpenCodeLongCatCatalog(body)
}

func fetchOpenCodePublicCatalog(ctx context.Context) ([]byte, error) {
	unavailable := model.Error("OPENCODE_CATALOG_UNAVAILABLE", "OpenCode public catalog is unavailable")
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, "/usr/bin/curl", "-q", "--proto", "=https", "--tlsv1.2", "--fail",
		"--silent", "--max-time", "18", "--max-filesize", "16000000", openCodePublicCatalog)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, unavailable
	}
	if err := cmd.Start(); err != nil {
		reader.Close()
		return nil, unavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, openCodePublicCatalogLimit+1))
	reader.Close()
	if err := cmd.Wait(); err != nil || readErr != nil || len(body) > openCodePublicCatalogLimit {
		return nil, unavailable
	}
	return body, nil
}

func longCatCatalogError() error {
	return model.Error("ZERO_COST_UNVERIFIED", "OpenCode LongCat public price evidence is unavailable or changed")
}

func projectOpenCodeLongCatCatalog(body []byte) ([]byte, error) {
	var catalog struct {
		OpenCode struct {
			ID     string                     `json:"id"`
			Name   string                     `json:"name"`
			Env    []string                   `json:"env"`
			Models map[string]json.RawMessage `json:"models"`
		} `json:"opencode"`
	}
	if json.Unmarshal(body, &catalog) != nil {
		return nil, longCatCatalogError()
	}
	p := catalog.OpenCode
	var entry any
	decoder := json.NewDecoder(bytes.NewReader(p.Models[openCodeLongCatModelID]))
	decoder.UseNumber()
	if decoder.Decode(&entry) != nil {
		return nil, longCatCatalogError()
	}
	projected, err := json.Marshal(map[string]any{"opencode": map[string]any{
		"id": p.ID, "name": p.Name, "env": p.Env, "models": map[string]any{openCodeLongCatModelID: entry},
	}})
	if err != nil || model.Hash(projected) != openCodeLongCatCatalogSHA256 {
		return nil, longCatCatalogError()
	}
	return projected, nil
}

// ProbeOpenCodePublicCatalog reports public inventory without admitting any model
// to the pilot. The catalog contains metadata, not observed quota balances.
func ProbeOpenCodePublicCatalog(ctx context.Context) ([]Quota, error) {
	body, err := fetchOpenCodePublicCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return openCodePublicQuota(body)
}

func openCodePublicQuota(body []byte) ([]Quota, error) {
	var catalog struct {
		OpenCode struct {
			Models map[string]struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"models"`
		} `json:"opencode"`
	}
	if json.Unmarshal(body, &catalog) != nil || len(catalog.OpenCode.Models) == 0 {
		return nil, model.Error("OPENCODE_CATALOG_UNAVAILABLE", "OpenCode public catalog has no valid model inventory")
	}
	var rows []Quota
	for id, entry := range catalog.OpenCode.Models {
		if id == entry.ID && model.ValidID(id) && entry.Status != "deprecated" {
			q := UnknownQuota("opencode")
			q.Model, q.OpenCodeProvider, q.Source = "opencode/"+id, "opencode", "public_catalog"
			rows = append(rows, q)
		}
	}
	if len(rows) == 0 {
		return nil, model.Error("OPENCODE_CATALOG_UNAVAILABLE", "OpenCode public catalog has no valid model inventory")
	}
	slices.SortFunc(rows, func(a, b Quota) int {
		return strings.Compare(a.Model, b.Model)
	})
	return rows, nil
}
