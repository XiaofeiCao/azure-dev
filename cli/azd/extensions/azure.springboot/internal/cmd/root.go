// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/azure/azure-dev/cli/azd/extensions/azure.springboot/internal/project"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

const version = "0.0.1"

// NewRootCommand creates the prototype command tree with caller-owned output.
func NewRootCommand(out, errOut io.Writer) *cobra.Command {
	root, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name: "springboot", Version: version,
		Short: "Build and configure the prepared Spring Boot PostgreSQL template.",
	})
	root.SetOut(out)
	root.SetErr(errOut)
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(&cobra.Command{
		Use: "metadata", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(
				azdext.GenerateExtensionMetadata("1.0", "azure.springboot", cmd.Root()))
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "version", Args: cobra.NoArgs,
		Short: "Display the locally built extension version.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	})
	configure := &cobra.Command{
		Use: "configure", Args: cobra.NoArgs,
		Short: "Validate and save developer-authenticated Spring PostgreSQL settings.",
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
			deployed, err := project.DeployedSpringSettings(values)
			if err != nil {
				return validationError(err)
			}
			maps.Copy(settings, deployed)
			certificate, err := developerCertificate(cmd.Context(), directory,
				values["POSTGRES_DEVELOPER_SSL_ROOT_CERT"], cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return fmt.Errorf("preparing developer PostgreSQL TLS trust: %w", err)
			}
			settings["POSTGRES_DEVELOPER_SSL_ROOT_CERT"] = certificate
			keys := slices.Sorted(maps.Keys(settings))
			for _, tracking := range []bool{false, true} {
				for _, key := range keys {
					if strings.HasPrefix(key, "SPRINGBOOT_PREVIOUS_") != tracking {
						continue
					}
					if _, err := client.Environment().SetValue(cmd.Context(), &azdext.SetEnvRequest{
						EnvName: extCtx.Environment, Key: key, Value: settings[key],
					}); err != nil {
						return fmt.Errorf("saving %s in the selected environment: %w", key, err)
					}
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(),
				"Spring Boot: validated and saved passwordless PostgreSQL configuration.")
			return err
		},
	}
	addProjectFlag(configure)
	root.AddCommand(configure, buildCommand(extCtx), runCommand(extCtx), applyCommand(extCtx))
	for _, subcommand := range root.Commands() {
		subcommand.PreRunE = func(cmd *cobra.Command, _ []string) error {
			if err := checkOutputFlag(cmd); err != nil {
				return err
			}
			if cmd.Name() == "build" {
				local, _ := cmd.Flags().GetBool("local-only")
				if local && cmd.Flag("environment").Changed {
					return validationError(fmt.Errorf("--environment cannot affect --local-only builds"))
				}
			}
			return nil
		}
	}
	return root
}

func addProjectFlag(cmd *cobra.Command) {
	cmd.Flags().String("project-dir", "", "Prepared template directory (defaults to the azd project directory)")
}

func sampleDirectory(cmd *cobra.Command) (string, error) {
	directory, err := cmd.Flags().GetString("project-dir")
	if err != nil {
		return "", err
	}
	if directory == "" {
		directory, err = azdext.GetProjectDir()
		if err != nil {
			return "", fmt.Errorf("finding the prepared project: %w", err)
		}
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	for _, file := range []string{"azure.yaml", filepath.Join("api", "pom.xml"), ".springboot-template"} {
		if info, err := os.Stat(filepath.Join(directory, file)); err != nil || !info.Mode().IsRegular() {
			return "", validationError(fmt.Errorf("project-dir must point to the prepared Spring Boot template"))
		}
	}
	return directory, nil
}

func environmentValues(ctx context.Context, name string) (*azdext.AzdClient, map[string]string, error) {
	client, err := azdext.NewAzdClient()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to azd; invoke this command through azd springboot: %w", err)
	}
	response, err := client.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{Name: name})
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("reading the selected azd environment: %w", err)
	}
	values := map[string]string{}
	for _, value := range response.KeyValues {
		values[value.Key] = value.Value
	}
	return client, values, nil
}

func requireHostProject(directory string) error {
	hostDirectory, err := azdext.GetProjectDir()
	if err != nil {
		return validationError(fmt.Errorf("select the prepared project with azd --cwd before using its environment"))
	}
	selected, err := os.Stat(directory)
	if err != nil {
		return err
	}
	host, err := os.Stat(hostDirectory)
	if err != nil {
		return fmt.Errorf("reading the azd host project directory: %w", err)
	}
	if !os.SameFile(selected, host) {
		return validationError(fmt.Errorf(
			"--project-dir must match the azd host project; use --cwd to select the same prepared template"))
	}
	return nil
}

func validationError(err error) error {
	return &azdext.LocalError{
		Message: err.Error(), Code: "invalid_configuration", Category: azdext.LocalErrorCategoryValidation,
		Suggestion: "Use the prepared template and align its explicit settings with the selected azd environment.",
	}
}
