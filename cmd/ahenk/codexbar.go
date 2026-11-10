// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func probeCodexBar(args []string) error {
	flags := flag.NewFlagSet("probe-codexbar", flag.ContinueOnError)
	path := flags.String("config", "", "Ahenk configuration with CodexBar enabled")
	provider := flags.String("provider", "", "codex or agy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || (*provider != "codex" && *provider != "agy") || flags.NArg() != 0 {
		return errors.New("probe-codexbar requires --config and --provider codex or agy")
	}
	cfg, err := model.LoadConfig(*path)
	if err != nil {
		return err
	}
	if !cfg.EnableCodexBar {
		return errors.New("CodexBar usage is disabled in this configuration")
	}
	var agy model.ProviderConfig
	for _, candidate := range cfg.Providers {
		if candidate.ID == "agy" {
			agy = candidate
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return json.NewEncoder(os.Stdout).Encode(providers.ReadCodexBar(ctx, *provider, agy, cfg.CodexBarAccountSHA256[*provider]))
}
