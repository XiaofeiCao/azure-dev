// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/azure/azure-dev/cli/azd/extensions/azure.springboot/internal/project"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

const builder = "paketobuildpacks/builder-noble-java-tiny@" +
	"sha256:b95da27fce97b58037f0c11ae934760c50730da4c9a24976205b53638592eba9"
const runImage = "paketobuildpacks/ubuntu-noble-run-tiny@" +
	"sha256:b1a26d91357134faa5a7f118f4e68a8c4c3278230660e8a49c924e92af6a905a"

var imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*:[A-Za-z0-9_.-]+$`)
var registryPattern = regexp.MustCompile(`^[a-z0-9]+\.azurecr\.io$`)

type runner func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error

func execute(ctx context.Context, name string, args []string, dir string, env []string, out, errOut io.Writer) error {
	if runtime.GOOS == "windows" && name == "az" {
		path, err := exec.LookPath(name)
		if err != nil {
			return fmt.Errorf("Azure CLI is required for registry and Container App operations: %w", err)
		}
		if strings.EqualFold(filepath.Ext(path), ".cmd") {
			// The MSI launcher forwards to Python; bypass cmd.exe so JDBC query '&' stays literal.
			python := filepath.Clean(filepath.Join(filepath.Dir(path), "..", "python.exe"))
			if _, err := os.Stat(python); err != nil {
				return fmt.Errorf("Azure CLI Python executable was not found beside the MSI launcher: %w", err)
			}
			name = python
			args = append([]string{"-IBm", "azure.cli"}, args...)
		}
	}
	process := exec.CommandContext(ctx, name, args...)
	process.Dir = dir
	process.Env = env
	process.Stdout = out
	process.Stderr = errOut
	process.Stdin = os.Stdin
	if err := process.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

func buildCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	command := &cobra.Command{
		Use: "build", Args: cobra.NoArgs,
		Short: "Build a Linux/AMD64 buildpack image and publish it to the template registry.",
		Long: "Build the prepared Java sample with pinned Cloud Native Buildpacks. " +
			"By default, publish to the provisioned registry and save SPRINGBOOT_IMAGE. " +
			"Use --local-only --image <tag> to build without Azure access.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			localOnly, _ := cmd.Flags().GetBool("local-only")
			image, _ := cmd.Flags().GetString("image")
			if localOnly && image == "" {
				return validationError(fmt.Errorf("--local-only requires --image"))
			}
			directory, err := sampleDirectory(cmd)
			if err != nil {
				return err
			}
			var client *azdext.AzdClient
			var values map[string]string
			registry := ""
			if !localOnly {
				if err := requireHostProject(directory); err != nil {
					return err
				}
				client, values, err = environmentValues(cmd.Context(), extCtx.Environment)
				if err != nil {
					return err
				}
				defer client.Close()
				registry = values["AZURE_CONTAINER_REGISTRY_ENDPOINT"]
				if !registryPattern.MatchString(registry) {
					return validationError(fmt.Errorf(
						"AZURE_CONTAINER_REGISTRY_ENDPOINT must be an Azure registry hostname"))
				}
				if image == "" {
					image = fmt.Sprintf("%s/springboot/notes:azd-%d", registry, time.Now().UnixNano())
				}
				if !strings.HasPrefix(image, registry+"/") {
					return validationError(fmt.Errorf("--image must target the provisioned registry"))
				}
			}
			if !imagePattern.MatchString(image) || strings.Contains(image, "..") {
				return validationError(fmt.Errorf("--image must be a tagged container image, not a credential-bearing URL"))
			}
			pack, _ := cmd.Flags().GetString("pack-path")
			if pack == "" {
				pack = os.Getenv("AZD_PACK_TOOL_PATH")
			}
			if pack == "" {
				pack = "pack"
			}
			if err := buildImage(cmd.Context(), execute, directory, pack, image,
				cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			if !localOnly {
				azArgs := []string{"acr", "login", "--name", strings.TrimSuffix(registry, ".azurecr.io")}
				if subscription := values["AZURE_SUBSCRIPTION_ID"]; subscription != "" {
					azArgs = append(azArgs, "--subscription", subscription)
				}
				if err := execute(cmd.Context(), "az", azArgs, directory, os.Environ(),
					cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
					return err
				}
				if err := execute(cmd.Context(), "docker", []string{"push", image}, directory, os.Environ(),
					cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
					return err
				}
				if _, err := client.Environment().SetValue(cmd.Context(), &azdext.SetEnvRequest{
					EnvName: extCtx.Environment, Key: "SPRINGBOOT_IMAGE", Value: image,
				}); err != nil {
					return fmt.Errorf("saving the published image: %w", err)
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Spring Boot: buildpack image ready: %s\n", image)
			return err
		},
	}
	addProjectFlag(command)
	command.Flags().Bool("local-only", false, "Build locally without registry login, upload, or azd environment changes")
	command.Flags().String("image", "",
		"Image tag; required for --local-only, otherwise defaults to the provisioned registry")
	command.Flags().String("pack-path", "", "Compatible pack executable (otherwise AZD_PACK_TOOL_PATH or PATH)")
	return command
}

func buildImage(ctx context.Context, run runner, directory, pack, image string, out, errOut io.Writer) error {
	if _, err := fmt.Fprintln(out,
		"Spring Boot: containerizing api with pinned Java 21 buildpacks (linux/amd64)."); err != nil {
		return err
	}
	if err := run(ctx, pack, []string{
		"build", image, "--path", filepath.Join(directory, "api"), "--builder", builder,
		"--run-image", runImage,
		"--platform", "linux/amd64", "--env", "BP_JVM_VERSION=21", "--trust-builder",
	}, directory, os.Environ(), out, errOut); err != nil {
		return fmt.Errorf("buildpack containerization (no Dockerfile fallback): %w", err)
	}
	var inspected strings.Builder
	if err := run(ctx, "docker", []string{"image", "inspect", image, "--format", "{{json .}}"},
		directory, os.Environ(), &inspected, errOut); err != nil {
		return err
	}
	var metadata struct{ Os, Architecture string }
	if err := json.Unmarshal([]byte(inspected.String()), &metadata); err != nil {
		return fmt.Errorf("reading built image metadata: %w", err)
	}
	if metadata.Os != "linux" || metadata.Architecture != "amd64" {
		return fmt.Errorf("buildpack image must be linux/amd64; got %s/%s", metadata.Os, metadata.Architecture)
	}
	return nil
}

func runCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	command := &cobra.Command{
		Use: "run", Args: cobra.NoArgs,
		Short: "Run the sample locally with selected-environment developer PostgreSQL settings.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			directory, err := sampleDirectory(cmd)
			if err != nil {
				return err
			}
			if err := requireHostProject(directory); err != nil {
				return err
			}
			client, values, err := environmentValues(cmd.Context(), extCtx.Environment)
			if err != nil {
				return err
			}
			defer client.Close()
			settings, err := project.SpringSettings(values)
			if err != nil {
				return validationError(err)
			}
			if tenant := values["AZURE_TENANT_ID"]; tenant != "" {
				settings["AZURE_TENANT_ID"] = tenant
			}
			certificate := values["POSTGRES_DEVELOPER_SSL_ROOT_CERT"]
			if err := validateCertificate(certificate); err != nil {
				return validationError(fmt.Errorf(
					"developer TLS configuration is missing or invalid; run configure: %w", err))
			}
			for _, key := range []string{"POSTGRES_HOST", "POSTGRES_DATABASE", "POSTGRES_PORT", "POSTGRES_USERNAME"} {
				if value := values[key]; value != "" {
					settings[key] = value
				}
			}
			settings["POSTGRES_SSL_ROOT_CERT"] = certificate
			settings["POSTGRES_MANAGED_IDENTITY_ENABLED"] = "false"
			settings["POSTGRES_SCHEMA"] = "notes"
			env := developerEnvironment(os.Environ(), settings)
			name, args := "./mvnw", []string{"spring-boot:run"}
			if runtime.GOOS == "windows" {
				name, args = "cmd.exe", []string{"/d", "/c", "mvnw.cmd", "spring-boot:run"}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Spring Boot: running locally with developer authentication.")
			if err != nil {
				return err
			}
			return execute(cmd.Context(), name, args, filepath.Join(directory, "api"), env,
				cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addProjectFlag(command)
	return command
}

func developerEnvironment(inherited []string, settings map[string]string) []string {
	env := make([]string, 0, len(inherited)+len(settings))
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		_, overridden := settings[key]
		if !overridden && key != "AZURE_CLIENT_ID" && key != "AZURE_CLIENT_SECRET" &&
			key != "AZURE_FEDERATED_TOKEN_FILE" && key != "SPRING_DATASOURCE_PASSWORD" {
			env = append(env, entry)
		}
	}
	for key, value := range settings {
		if !strings.HasPrefix(key, "SPRINGBOOT_PREVIOUS_") {
			env = append(env, key+"="+value)
		}
	}
	return env
}
