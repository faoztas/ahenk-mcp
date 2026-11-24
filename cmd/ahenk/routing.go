// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const routingUsage = "ahenk routing {get,set,reset,web} --config FILE --repo ID [PROFILE]"
const routingConfigError = "cannot load routing configuration"

func runRouting(args []string) error {
	if len(args) == 0 {
		return errors.New(routingUsage)
	}
	command := args[0]
	wantArgs := 0
	switch command {
	case "web":
		return runRoutingWeb(args[1:])
	case "set":
		wantArgs = 1
	case "get", "reset":
	default:
		return errors.New(routingUsage)
	}
	flags := flag.NewFlagSet("routing "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "path to local configuration")
	repoID := flags.String("repo", "", "configured repository ID")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New(routingUsage)
	}
	if *path == "" || *repoID == "" || flags.NArg() != wantArgs {
		return errors.New(routingUsage)
	}
	var preference *string
	if command == "set" {
		value := flags.Arg(0)
		if !model.RoutingProfile(value).Valid() {
			return fmt.Errorf("routing profile must be one of %v", model.RoutingProfiles())
		}
		preference = &value
	}
	cfg, err := model.LoadConfig(*path)
	if err != nil {
		return errors.New(routingConfigError)
	}
	repo, ok := cfg.Repositories[*repoID]
	if !ok {
		return errors.New("repository is not registered")
	}
	stateDir, err := store.RoutingPreferenceStateDir(cfg)
	if err != nil {
		return err
	}
	if command != "get" {
		if err := store.SaveRoutingPreference(stateDir, *repoID, repo.Root, preference); err != nil {
			if errors.Is(err, store.ErrInvalidRoutingPreference) {
				return store.ErrInvalidRoutingPreference
			}
			return errors.New("cannot save routing preference")
		}
	}
	profile, source, err := orchestration.ResolveDefaultRoutingProfile(stateDir, *repoID, repo)
	if err != nil {
		if !errors.Is(err, store.ErrInvalidRoutingPreference) {
			return errors.New("cannot read routing preference")
		}
		fmt.Fprintln(os.Stderr, "ahenk: ignoring invalid operator routing preference")
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		RepoID  string               `json:"repo_id"`
		Profile model.RoutingProfile `json:"routing_profile,omitempty"`
		Source  string               `json:"routing_profile_source"`
	}{RepoID: *repoID, Profile: profile, Source: source}); err != nil {
		return errors.New("cannot write routing result")
	}
	return nil
}
