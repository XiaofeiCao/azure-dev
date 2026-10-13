// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
)

func TestApplyPreparesCLIWithoutEnvironmentArguments(t *testing.T) {
	for _, failPreparation := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "preparation failure"}[failPreparation], func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := []string{"containerapp", "update", "--set-env-vars",
				"SPRING_DATASOURCE_URL=jdbc:postgresql://sample.postgres.database.azure.com:5432/notes" +
					"?authenticationPluginClassName=plugin&currentSchema=notes&sslmode=verify-full",
				"SPRING_DATASOURCE_USERNAME=app", "POSTGRES_SCHEMA=notes", "--output", "none"}
			calls := 0
			failure := errors.New("CLI preparation failed")
			run := func(_ context.Context, name string, actual []string, _ string,
				_ []string, stdout, stderr io.Writer) error {
				calls++
				if name != "az" || stderr != &errOut {
					t.Fatal("CLI bypassed its injected stderr")
				}
				if calls == 1 {
					if !slices.Equal(actual, []string{"containerapp", "update", "--help"}) || stdout != io.Discard {
						t.Fatal("CLI preparation received runtime settings or forwarded command help")
					}
					if failPreparation {
						return failure
					}
				} else if !slices.Equal(actual, args) || stdout != &out {
					t.Fatal("environment names/values changed or bypassed the injected writer")
				}
				return nil
			}
			err := applyContainerAppSettings(t.Context(), run, args, &out, &errOut)
			if failPreparation {
				if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("preparation error=%v calls=%d", err, calls)
				}
			} else if err != nil || calls != 2 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestExecutePreservesEnvironmentArgumentBoundaries(t *testing.T) {
	expected := []string{
		"SPRING_DATASOURCE_URL=jdbc:postgresql://sample/notes?authenticationPluginClassName=plugin" +
			"&currentSchema=notes&sslmode=verify-full",
		"SPRING_DATASOURCE_USERNAME=app", "POSTGRES_SCHEMA=notes",
	}
	if os.Getenv("SPRINGBOOT_ARGUMENT_TRANSPORT_TEST") == "1" {
		if len(os.Args) < 3 || !slices.Equal(os.Args[3:], expected) {
			t.Fatal("native process transport changed environment argument boundaries")
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-test.run=^TestExecutePreservesEnvironmentArgumentBoundaries$", "--"}, expected...)
	env := append(os.Environ(), "SPRINGBOOT_ARGUMENT_TRANSPORT_TEST=1")
	var out, errOut bytes.Buffer
	if err := execute(t.Context(), executable, args, t.TempDir(), env, &out, &errOut); err != nil {
		t.Fatalf("argument transport failed: %v; %s%s", err, out.String(), errOut.String())
	}
}
