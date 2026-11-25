// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exampleBinary = "/absolute/path/to/ahenk-mcp/bin/ahenk"
	exampleConfig = "/absolute/path/to/ahenk.json"
)

type clientExample struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Type    string   `json:"type"`
}

func readClientExample(t *testing.T, name string) clientExample {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	var example clientExample
	if filepath.Ext(name) == ".toml" {
		// The checked-in TOML example uses one table and JSON-compatible
		// literals. Decode this subset without a client or TOML dependency.
		pattern := regexp.MustCompile(`^\[mcp_servers\.ahenk\][ \t]*\r?\ncommand[ \t]*=[ \t]*("[^"\\\r\n]*")[ \t]*\r?\nargs[ \t]*=[ \t]*(\[[^\\\r\n]*\])[ \t]*$`)
		fields := pattern.FindSubmatch(bytes.Trim(data, " \t\r\n"))
		if fields == nil {
			t.Fatalf("%s must define command and args under [mcp_servers.ahenk]", name)
		}
		if err := json.Unmarshal(fields[1], &example.Command); err != nil {
			t.Fatalf("%s command: %v", name, err)
		}
		if err := json.Unmarshal(fields[2], &example.Args); err != nil {
			t.Fatalf("%s args: %v", name, err)
		}
	} else {
		var config struct {
			Servers map[string]clientExample `json:"mcpServers"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			t.Fatalf("%s: unexpected content after JSON object", name)
		}
		example = config.Servers["ahenk"]
		if len(config.Servers) != 1 || example.Type != "stdio" {
			t.Fatalf("%s must define one stdio server at mcpServers.ahenk", name)
		}
	}
	wantArgs := []string{"serve", "--stdio", "--config", exampleConfig}
	if example.Command != exampleBinary || !slices.Equal(example.Args, wantArgs) {
		t.Fatalf("%s: got command %q and args %q, want command %q and args %q", name, example.Command, example.Args, exampleBinary, wantArgs)
	}
	return example
}

func TestClientExamplesLaunchCLIStdio(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "Operator Settings", "Fixture Configs")
	binaryDir := filepath.Join(root, "Ahenk Tools")
	repoDir := filepath.Join(root, "registered repo")
	for _, dir := range []string{configDir, binaryDir, repoDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(binaryDir, "ahenk")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// TestMain dispatches serve to the production entry point.
	if err := os.Symlink(testBinary, binary); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(configDir, "ahenk fixture.json")
	badConfigPath := filepath.Join(configDir, "bad config.json")
	cfg, err := json.Marshal(model.Config{
		StateDir:      filepath.Join(root, "state"),
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {Root: repoDir, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badConfigPath, []byte(`{"state_dir":"relative"}`), 0600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"codex-ahenk.toml", "claude-ahenk.mcp.json"} {
		t.Run(name, func(t *testing.T) {
			example := readClientExample(t, name)
			workingDir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := func(configPath string) *exec.Cmd {
				args := slices.Clone(example.Args)
				args[3] = strings.ReplaceAll(args[3], exampleConfig, configPath)
				cmd := exec.CommandContext(ctx, strings.ReplaceAll(example.Command, exampleBinary, binary), args...)
				cmd.Dir = workingDir
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
				return cmd
			}

			var stderr bytes.Buffer
			cmd := command(cfgPath)
			cmd.Stderr = &stderr
			client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
			if err != nil {
				t.Fatalf("initialize: %v; stderr: %s", err, stderr.String())
			}
			defer func() {
				if err := session.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			if got := session.InitializeResult().ServerInfo.Name; got != "ahenk" {
				t.Fatalf("initialized unexpected server %q", got)
			}
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			for _, name := range []string{"delegate_task", "list_capabilities"} {
				if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == name }) {
					t.Errorf("tools/list missing %q", name)
				}
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "list_capabilities",
				Arguments: map[string]any{"repo_id": "demo", "provider": "fixture-a"},
			})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("list_capabilities: %v, result: %+v", err, result)
			}
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var capabilities struct {
				SchedulerStatus string             `json:"scheduler_status"`
				Providers       []model.Capability `json:"providers"`
			}
			if err := json.Unmarshal(data, &capabilities); err != nil {
				t.Fatal(err)
			}
			if capabilities.SchedulerStatus != "active" || !slices.ContainsFunc(capabilities.Providers, func(provider model.Capability) bool {
				return provider.Provider == "fixture-a" && provider.Status == "available"
			}) {
				t.Fatalf("expected active scheduler and configured fixture route, got %+v", capabilities)
			}

			bad := command(badConfigPath)
			var badStderr bytes.Buffer
			bad.Stderr = &badStderr
			stdout, err := bad.Output()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(stdout) != 0 ||
				!strings.Contains(badStderr.String(), "state_dir must be absolute") {
				t.Fatalf("bad config: err=%v stdout=%q stderr=%q", err, stdout, badStderr.String())
			}
		})
	}
}
