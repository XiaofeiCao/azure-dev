// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildImage(t *testing.T) {
	for _, test := range []struct {
		name    string
		inspect string
		fail    bool
	}{
		{"linux amd64", `{"Os":"linux","Architecture":"amd64"}`, false},
		{"wrong architecture", `{"Os":"linux","Architecture":"arm64"}`, true},
		{"wrong OS", `{"Os":"windows","Architecture":"amd64"}`, true},
		{"malformed inspect", `{`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			calls := 0
			run := func(_ context.Context, name string, args []string, dir string,
				env []string, stdout, stderr io.Writer) error {
				calls++
				if calls == 1 {
					if name != "pack-tool" || !slices.Contains(args, "--platform") ||
						!slices.Contains(args, "linux/amd64") || !slices.Contains(args, builder) ||
						!slices.Contains(args, filepath.Join("sample", "api")) {
						t.Fatalf("invalid pack invocation: %s %v", name, args)
					}
					if stdout != &out || stderr != &errOut {
						t.Fatal("pack bypassed injected writers")
					}
				} else {
					if name != "docker" || stderr != &errOut {
						t.Fatal("inspection bypassed injected stderr")
					}
					_, err := io.WriteString(stdout, test.inspect)
					return err
				}
				return nil
			}
			err := buildImage(t.Context(), run, "sample", "pack-tool", "notes:test", &out, &errOut)
			if (err != nil) != test.fail || calls != 2 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if !strings.Contains(out.String(), "containerizing api") {
				t.Fatal("extension participation missing from injected output")
			}
		})
	}
}

func TestBuildImageDoesNotFallBack(t *testing.T) {
	calls := 0
	run := func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error {
		calls++
		return errors.New("pack failed")
	}
	err := buildImage(t.Context(), run, "sample", "pack", "notes:test", io.Discard, io.Discard)
	if err == nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestDeveloperEnvironment(t *testing.T) {
	env := developerEnvironment([]string{
		"PATH=tools", "AZURE_CLIENT_ID=managed", "AZURE_CLIENT_SECRET=secret",
		"AZURE_FEDERATED_TOKEN_FILE=token", "SPRING_DATASOURCE_PASSWORD=password", "SPRING_DATASOURCE_USERNAME=old",
	}, map[string]string{"SPRING_DATASOURCE_USERNAME": "developer"})
	if !slices.Contains(env, "PATH=tools") || !slices.Contains(env, "SPRING_DATASOURCE_USERNAME=developer") {
		t.Fatal("missing developer settings or inherited tool path")
	}
	for _, entry := range env {
		if strings.Contains(entry, "secret") || strings.Contains(entry, "password") ||
			strings.Contains(entry, "managed") || strings.Contains(entry, "old") {
			t.Fatal("deployed credentials leaked into developer runtime")
		}
	}
}

func TestVersionUsesInjectedWriter(t *testing.T) {
	var out bytes.Buffer
	command := NewRootCommand(&out, io.Discard)
	command.SetArgs([]string{"version"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("version output=%q", out.String())
	}
}

func TestBuildFlagValidation(t *testing.T) {
	command := NewRootCommand(io.Discard, io.Discard)
	command.SetArgs([]string{"build", "--local-only"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("error=%v", err)
	}
}

func TestSampleDirectoryRequiresMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "api"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"azure.yaml", filepath.Join("api", "pom.xml")} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("sample"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := NewRootCommand(io.Discard, io.Discard)
	build, _, err := command.Find([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if err := build.Flags().Set("project-dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := sampleDirectory(build); err == nil {
		t.Fatal("unrelated Java project was accepted")
	}
}

func TestExplicitUnsupportedFlagsFail(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"output", []string{"version", "--output", "json"}, "--output"},
		{"local environment", []string{"build", "--local-only", "--image", "notes:test", "-e", "ignored"},
			"--environment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := NewRootCommand(io.Discard, io.Discard)
			command.SetArgs(test.args)
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestExecuteUsesInjectedWriters(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute(t.Context(), "go", []string{"version"}, t.TempDir(), os.Environ(), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "go version") {
		t.Fatal("child output did not reach the injected writer")
	}
}

func TestCredentialBearingImageIsNotDisclosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "api"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"azure.yaml", filepath.Join("api", "pom.xml"), ".springboot-template"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("test fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	command := NewRootCommand(&out, &errOut)
	command.SetArgs([]string{
		"build", "--local-only", "--project-dir", dir,
		"--image", "https://sample-user:sample-password@example.com/image:tag?sig=sample-token#sample-fragment",
	})
	err := command.Execute()
	if err == nil {
		t.Fatal("credential-bearing image was accepted")
	}
	combined := out.String() + errOut.String() + err.Error()
	for _, sensitive := range []string{"sample-user", "sample-password", "sample-token", "sample-fragment"} {
		if strings.Contains(combined, sensitive) {
			t.Fatal("credential-bearing URL was disclosed")
		}
	}
}
