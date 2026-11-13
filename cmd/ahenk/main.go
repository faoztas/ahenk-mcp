// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/ipc"
	"github.com/faoztas/ahenk-mcp/internal/mcpserver"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/readiness"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	syscall.Umask(0077)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ahenk:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ahenk serve --stdio --config FILE | ahenk readiness --config FILE | ahenk m1-request --config FILE --repo ID | ahenk verify-m1 --config FILE --request FILE --job ID | ahenk local-test --config FILE | ahenk version")
	}
	switch args[0] {
	case "local-test":
		return localTest(args[1:])
	case "_supervise_local_swift":
		return execution.SuperviseLocalSwift()
	case "private-brief-digest":
		return privateBriefDigest(args[1:])
	case "audit-models":
		return auditModels(args[1:])
	case "probe-codexbar":
		return probeCodexBar(args[1:])
	case "probe-opencode-catalog":
		return probeOpenCodeCatalog(args[1:])
	case "import-opencode-cooldown":
		return importOpenCodeCooldown(args[1:])
	case "m1-request", "verify-m1":
		return runM1(args[0], args[1:])
	case "readiness":
		flags := flag.NewFlagSet("readiness", flag.ContinueOnError)
		path := flags.String("config", "", "path to Ahenk configuration")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || flags.NArg() != 0 {
			return errors.New("readiness requires --config")
		}
		cfg, err := model.LoadConfig(*path)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		report, err := readiness.Read(ctx, cfg, time.Now().UTC())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	case "probe-agy":
		flags := flag.NewFlagSet("probe-agy", flag.ContinueOnError)
		path := flags.String("config", "", "path to Ahenk configuration with an audited AGY binary")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || flags.NArg() != 0 {
			return errors.New("probe-agy requires --config")
		}
		cfg, err := model.LoadConfig(*path)
		if err != nil {
			return err
		}
		for _, provider := range cfg.Providers {
			if provider.ID != "agy" {
				continue
			}
			dir, err := os.MkdirTemp("", "ahenk-agy-usage-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			evidence, err := providers.ProbeAGY(ctx, provider, dir)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(evidence)
		}
		return errors.New("AGY is not configured")
	case "decode-quota":
		flags := flag.NewFlagSet("decode-quota", flag.ContinueOnError)
		provider := flags.String("provider", "", "CodexBar export provider")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("decode-quota reads JSON from stdin")
		}
		rows, err := providers.DecodeCodexBar(os.Stdin, *provider, time.Now().UTC())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "version", "--version":
		fmt.Println("ahenk", model.Version, providers.Platform())
		return nil
	case "_supervise_go_check":
		return execution.SuperviseGoChecks()
	case "_supervise_agy":
		return providers.SuperviseAGY()
	case "_supervise_fixture":
		return execution.SuperviseFixture()
	case "_fixture_worker":
		return execution.FixtureWorker()
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		path := flags.String("config", "", "absolute path to local configuration")
		stdio := flags.Bool("stdio", false, "serve MCP over stdio")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !*stdio || *path == "" || flags.NArg() != 0 {
			return errors.New("serve requires --stdio and --config")
		}
		return serve(*path)
	default:
		return errors.New("unknown command")
	}
}

func serve(path string) error {
	cfg, err := model.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	db, err := store.Open(cfg.StateDir)
	if errors.Is(err, store.ErrBusy) {
		return ipc.RunProxy(ctx, cfg.StateDir, os.Stdin, os.Stdout)
	}
	if err != nil {
		return err
	}
	defer db.Close()
	reg := providers.New(cfg)
	engine, err := orchestration.New(cfg, db, reg)
	if err != nil {
		return err
	}
	defer engine.Close()
	server := mcpserver.New(engine, cfg, reg)

	daemon, err := ipc.StartDaemon(ctx, cfg.StateDir, server)
	if err != nil {
		return err
	}
	defer daemon.Close()

	return runServer(ctx, server)
}

func runServer(ctx context.Context, s *mcp.Server) error {
	err := s.Run(ctx, &mcp.StdioTransport{MaxLineLength: 512 << 10})
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
