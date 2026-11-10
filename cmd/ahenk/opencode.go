// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func probeOpenCodeCatalog(args []string) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "--longcat" && args[0] != "--public" {
		return errors.New("probe-opencode-catalog accepts only --longcat or --public")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	if len(args) == 1 && args[0] == "--public" {
		rows, err := providers.ProbeOpenCodePublicCatalog(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Quota []providers.Quota `json:"quota"`
		}{Quota: rows})
	}
	dir, err := os.MkdirTemp("", "ahenk-opencode-catalog-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var probe providers.OpenCodeCatalogProbe
	if len(args) == 1 {
		probe, err = providers.ProbeOpenCodeLongCatCatalog(ctx, dir)
	} else {
		probe, err = providers.ProbeOpenCodeCatalog(ctx, dir)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(probe)
}

func importOpenCodeCooldown(args []string) error {
	flags := flag.NewFlagSet("import-opencode-cooldown", flag.ContinueOnError)
	path := flags.String("config", "", "Ahenk configuration for the existing destination state")
	var sources []string
	flags.Func("from-state", "absolute path to a historical Ahenk state directory (repeatable)", func(source string) error {
		if !filepath.IsAbs(source) {
			return errors.New("source state path must be absolute")
		}
		sources = append(sources, source)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || len(sources) == 0 || flags.NArg() != 0 {
		return errors.New("import-opencode-cooldown requires --config and --from-state")
	}
	cfg, err := model.LoadConfig(*path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(cfg.StateDir, "state.sqlite"))
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("destination must contain an existing Ahenk database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var records []store.OpenCodeCooldownImport
	now := time.Now().UTC()
	for _, source := range sources {
		rows, err := store.ReadOpenCodeRateLimits(ctx, source)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errors.New("source contains no OpenCode rate-limit records")
		}
		for _, r := range rows {
			if !providers.OpenCodeIdentityMatches(r.Model, r.Fingerprint) || r.FinishedAt.After(now) {
				return errors.New("unverified OpenCode cooldown source record")
			}
		}
		records = append(records, rows...)
	}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer db.Close()
	imported, err := db.SaveOpenCodeCooldownImports(records, now)
	if err != nil {
		return err
	}
	type gateKey struct {
		Scope   string `json:"scope"`
		Model   string `json:"model,omitempty"`
		Account string `json:"opencode_account"`
	}
	type gate struct {
		gateKey
		RetryAt time.Time `json:"retry_at"`
	}
	byScope := make(map[gateKey]time.Time)
	for _, r := range imported {
		key := gateKey{Scope: orchestration.OpenCodeRateLimitScope(r.ProviderRateLimitScope), Account: model.OpenCodeAccountID(r.OpenCodeAccount)}
		if key.Scope == "model" {
			key.Model = r.Model
		}
		if until := orchestration.OpenCodeImportRetryAt(r); until.After(byScope[key]) {
			byScope[key] = until
		}
	}
	gates := make([]gate, 0, len(byScope))
	for key, until := range byScope {
		gates = append(gates, gate{gateKey: key, RetryAt: until})
	}
	slices.SortFunc(gates, func(a, b gate) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Account, b.Account))
	})
	return json.NewEncoder(os.Stdout).Encode(struct {
		Count int    `json:"imported_source_attempts"`
		Gates []gate `json:"gates"`
	}{Count: len(imported), Gates: gates})
}
