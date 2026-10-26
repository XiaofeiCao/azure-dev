// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"azure.springboot/internal/spring"
)

func TestRunPreflight(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		message string
	}{
		{"streamed output", []string{"run", "--output", "json"}, "--output"},
		{"explicit default output", []string{"run", "--output", "none"}, "--output"},
		{"explicit empty output", []string{"run", "--output="}, "--output"},
		{"explicit empty environment", []string{"run", "--environment="}, "non-empty azd environment"},
		{"blank environment", []string{"run", "--environment", " "}, "non-empty azd environment"},
		{"extra arguments", []string{"run", "extra"}, "unknown command"},
		{"no trust flag", []string{"run", "--yes"}, "unknown flag"},
		{"missing project without prompts", []string{"run", "--no-prompt"}, "azd spring init"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("AZD_CWD", "")
			t.Setenv("AZD_NO_PROMPT", "")
			command := NewRootCommand()
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetIn(strings.NewReader(""))
			command.SetArgs(tt.args)
			require.ErrorContains(t, command.ExecuteContext(t.Context()), tt.message)
			require.NotContains(t, output.String(), "Trust")
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Setenv("AZD_CWD", "")
	command := NewRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"run", "--help"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	require.Contains(t, output.String(), "run")
	require.Contains(t, output.String(), "PostgreSQL")
}

func TestRunEmbeddedCommand(t *testing.T) {
	directory := t.TempDir()
	plan, err := spring.Generate(t.Context(), &spring.Application{
		Name: "inventory", JavaVersion: 17, JarName: "inventory.jar", BuildDirectory: "target",
		Port: 9090, Profiles: []string{"release"},
	})
	require.NoError(t, err)
	require.NoError(t, plan.Write(t.Context(), directory))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "pom.xml"), []byte("<project/>"), 0600))
	wrapper, script := "mvnw", "#!/bin/sh\nread input\necho \"application:$input\"\necho diagnostic >&2\n"
	if runtime.GOOS == "windows" {
		wrapper = "mvnw.cmd"
		script = "@echo off\r\nset /p INPUT=\r\necho application:%INPUT%\r\necho diagnostic 1>&2\r\n"
	}
	file := filepath.Join(directory, wrapper)
	require.NoError(t, os.WriteFile(file, []byte(script), 0600))
	require.NoError(t, os.Chmod(file, 0700)) //nolint:gosec // Execute this test's Maven wrapper.
	t.Chdir(t.TempDir())
	t.Setenv("AZD_CWD", "")
	t.Setenv("AZD_ENVIRONMENT", "inherited-environment-does-not-require-Azure")
	t.Setenv("SPRING_PROFILES_ACTIVE", "release")
	t.Setenv("SPRING_APPLICATION_JSON", "")
	t.Setenv("SERVER_PORT", "9090")
	var output, diagnostic, ambient bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&ambient)
	defer log.SetOutput(previous)
	command := NewRootCommand()
	command.SetOut(&output)
	command.SetErr(&diagnostic)
	command.SetIn(strings.NewReader("input\n"))
	command.SetArgs([]string{"run", "--cwd", directory, "--no-prompt", "--debug"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	require.Contains(t, output.String(), "application:input")
	require.Contains(t, diagnostic.String(), "diagnostic")
	require.Empty(t, ambient.String())
	require.NotContains(t, output.String(), "Trust")

	command = NewRootCommand()
	command.SetOut(&output)
	command.SetErr(&diagnostic)
	command.SetArgs([]string{"run", "--environment", "explicit"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "--environment")
}
