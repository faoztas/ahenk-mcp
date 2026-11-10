// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/readiness"
)

func runM1(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the canonical Ahenk configuration")
	var repoID, requestPath, parentID string
	if command == "m1-request" {
		flags.StringVar(&repoID, "repo", "", "dedicated synthetic repository ID")
	} else {
		flags.StringVar(&requestPath, "request", "", "path to the original m1-request JSON")
		flags.StringVar(&parentID, "job", "", "parent ID returned by delegate_batch")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 || command == "m1-request" && repoID == "" ||
		command == "verify-m1" && (requestPath == "" || parentID == "") {
		return errors.New("m1-request requires --config and --repo. verify-m1 requires --config, --request and --job")
	}
	cfg, err := model.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	out := json.NewEncoder(os.Stdout)
	if command == "m1-request" {
		request, err := readiness.PrepareM1(ctx, cfg, repoID, now)
		if err != nil {
			return err
		}
		return out.Encode(request)
	}
	request, err := readM1Request(requestPath)
	if err != nil {
		return err
	}
	evidence, err := readiness.VerifyM1(ctx, cfg, request, parentID, now)
	if err != nil {
		return err
	}
	return out.Encode(evidence)
}

func readM1Request(path string) (model.Batch, error) {
	var request model.Batch
	f, err := os.Open(path)
	if err != nil {
		return request, err
	}
	defer f.Close()
	const limit = 16 << 10
	in := &io.LimitedReader{R: f, N: limit + 1}
	d := json.NewDecoder(in)
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || in.N == 0 {
		return request, errors.New("invalid M1 request JSON")
	}
	return request, nil
}
