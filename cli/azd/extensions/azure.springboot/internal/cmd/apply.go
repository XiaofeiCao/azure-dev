// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/azure/azure-dev/cli/azd/extensions/azure.springboot/internal/project"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

var resourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.()-]{0,89}$`)
var clientID = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

func applyCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	command := &cobra.Command{
		Use: "apply", Args: cobra.NoArgs,
		Short: "Apply validated managed-identity Spring settings to the provisioned Container App.",
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
			settings, err := project.DeployedSpringSettings(values)
			if err != nil {
				return validationError(err)
			}
			for _, key := range []string{"AZURE_CONTAINER_APP_NAME", "AZURE_RESOURCE_GROUP"} {
				if !resourceName.MatchString(values[key]) {
					return validationError(fmt.Errorf("%s must name the provisioned resource", key))
				}
			}
			if !clientID.MatchString(values["AZURE_CLIENT_ID"]) {
				return validationError(fmt.Errorf("AZURE_CLIENT_ID must identify the application's managed identity"))
			}
			args := []string{"containerapp", "update", "--name", values["AZURE_CONTAINER_APP_NAME"],
				"--resource-group", values["AZURE_RESOURCE_GROUP"], "--set-env-vars"}
			for _, key := range []string{"SPRING_DATASOURCE_URL", "SPRING_DATASOURCE_USERNAME", "SPRING_PROFILES_ACTIVE"} {
				value := settings["SPRINGBOOT_DEPLOYED_"+key]
				if values["SPRINGBOOT_DEPLOYED_"+key] != value {
					return validationError(fmt.Errorf("run configure before apply; %s is missing or stale", key))
				}
				args = append(args, key+"="+value)
			}
			args = append(args, "AZURE_CLIENT_ID="+values["AZURE_CLIENT_ID"],
				"POSTGRES_MANAGED_IDENTITY_ENABLED=true", "POSTGRES_SSL_ROOT_CERT=/etc/ssl/certs/ca-certificates.crt",
				"POSTGRES_SCHEMA=notes", "POSTGRES_USERNAME="+values["POSTGRES_MANAGED_IDENTITY_USERNAME"])
			for _, key := range []string{"POSTGRES_HOST", "POSTGRES_DATABASE", "POSTGRES_PORT"} {
				if value := values[key]; value != "" {
					args = append(args, key+"="+value)
				}
			}
			args = append(args, "--output", "none")
			if subscription := values["AZURE_SUBSCRIPTION_ID"]; subscription != "" {
				args = append(args, "--subscription", subscription)
			}
			if err := applyContainerAppSettings(cmd.Context(), execute, args,
				cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(),
				"Spring Boot: applied extension-generated managed-identity configuration to the Container App.")
			return err
		},
	}
	addProjectFlag(command)
	return command
}

func applyContainerAppSettings(ctx context.Context, run runner, args []string, out, errOut io.Writer) error {
	// Older Azure CLI releases restart through cmd.exe after automatic extension installation.
	if err := run(ctx, "az", []string{"containerapp", "update", "--help"}, "", os.Environ(),
		io.Discard, errOut); err != nil {
		return fmt.Errorf("preparing the Azure CLI Container Apps command: %w", err)
	}
	return run(ctx, "az", args, "", os.Environ(), out, errOut)
}

func checkOutputFlag(cmd *cobra.Command) error {
	value, _ := cmd.Flags().GetString("output")
	if value != "" && value != "default" {
		return validationError(fmt.Errorf(
			"--output %s is not supported by this prototype command", strings.TrimSpace(value)))
	}
	return nil
}
