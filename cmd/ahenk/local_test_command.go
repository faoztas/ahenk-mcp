// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
)

func localTest(args []string) error {
	flags := flag.NewFlagSet("local-test", flag.ContinueOnError)
	config := flags.String("config", "", "path to a private local Swift configuration")
	timeout := flags.Duration("timeout", 8*time.Minute, "maximum test duration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *config == "" || *timeout <= 0 || *timeout > 18*time.Minute || flags.NArg() != 0 {
		return errors.New("local-test requires --config and a timeout up to 15 minutes")
	}
	owner, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(owner, *timeout)
	defer cancel()
	result, err := execution.RunLocalSwift(ctx, *config)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if result.Status != "PASSED" {
		return errors.New("local Swift verification did not pass")
	}
	return nil
}
