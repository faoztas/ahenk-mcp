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
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func auditModels(args []string) error {
	flags := flag.NewFlagSet("audit-models", flag.ContinueOnError)
	path := flags.String("config", "", "absolute path to the authorized Ahenk configuration")
	live := flags.Bool("live", false, "run one bounded synthetic inference per eligible exact model")
	previousPath := flags.String("previous", "", "reuse an earlier inventory from this audit window")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*path) || flags.NArg() != 0 {
		return errors.New("audit-models requires --config with an absolute path")
	}
	cfg, err := model.LoadConfig(*path)
	if err != nil {
		return err
	}
	var previous model.ModelInventory
	if *previousPath != "" {
		if !filepath.IsAbs(*previousPath) {
			return errors.New("audit previous inventory must use an absolute path")
		}
		inventory, err := model.LoadModelInventory(*previousPath)
		if err != nil {
			return err
		}
		previous = *inventory
		if !previous.Fresh(time.Now().UTC()) {
			return errors.New("audit previous inventory is stale")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cooldown, err := auditCooldown(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	type observation struct {
		rows []model.ModelAudit
		err  error
	}
	collected := make(chan observation, 2)
	var count int
	if i := slices.IndexFunc(cfg.Providers, func(p model.ProviderConfig) bool { return p.ID == "agy" }); i >= 0 {
		count++
		go func() {
			rows, err := providers.AuditAGY(ctx, cfg.Providers[i], *live, previous.Models)
			collected <- observation{rows, err}
		}()
	}
	if slices.ContainsFunc(cfg.Providers, func(p model.ProviderConfig) bool {
		return p.ID == "opencode" && p.EnableSynthetic && providers.OpenCodeIdentityMatches(p.Model, p.ExpectedBinarySHA256)
	}) {
		count++
		go func() {
			rows, err := providers.AuditOpenCode(ctx, *live, cooldown, previous.Models)
			collected <- observation{rows, err}
		}()
	}
	if count == 0 {
		return errors.New("no authorized audit provider is configured")
	}
	inventory := model.ModelInventory{Fixture: model.ModelAuditFixture, ObservedAt: time.Now().UTC()}
	if !previous.ObservedAt.IsZero() {
		inventory.ObservedAt = previous.ObservedAt
	}
	var failure error
	for range count {
		next := <-collected
		failure = errors.Join(failure, next.err)
		inventory.Models = append(inventory.Models, next.rows...)
	}
	slices.SortFunc(inventory.Models, func(a, b model.ModelAudit) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Model, b.Model))
	})
	for i := range inventory.Models {
		row := &inventory.Models[i]
		if row.LatencyMS > 0 || row.Terminal {
			row.EvidenceID = inventory.EvidenceID(*row)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return errors.Join(failure, enc.Encode(inventory))
}

func auditCooldown(ctx context.Context, state string) (providers.AuditCooldown, error) {
	if _, err := os.Stat(filepath.Join(state, "state.sqlite")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	records, err := store.ReadOpenCodeRateLimits(ctx, state)
	if err != nil {
		return nil, err
	}
	return func(id string, now time.Time) *time.Time {
		var latest *time.Time
		for _, r := range records {
			c := model.Capability{Model: id, OpenCodeAccount: model.OpenCodeAnonymous, OpenCodeTransport: model.OpenCodeNativeReadAsk}
			scope := orchestration.OpenCodeRateLimitScope(r.ProviderRateLimitScope)
			if !orchestration.OpenCodeGateApplies(scope, r.Model, r.OpenCodeAccount, r.OpenCodeTransport, c) {
				continue
			}
			if at := orchestration.OpenCodeImportRetryAt(r); now.Before(at) && (latest == nil || at.After(*latest)) {
				latest = &at
			}
		}
		return latest
	}, nil
}
