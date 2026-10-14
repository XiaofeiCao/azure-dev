// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"

	"azure.springboot/internal/spring"
)

func newRunCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run a generated Spring Boot Maven application locally",
		Long: `Run a trusted single-module Maven application prepared by azd spring init.
Embedded applications need no Azure login or provisioning. PostgreSQL applications
use the current provisioned azd environment and require secret-read access to its
Key Vault. Local and deployed applications in that environment share database data.

This command executes the project's Maven wrapper or Maven on PATH. It does not
provision or deploy resources. MySQL, managed identity, and extra Maven/application
arguments are not supported. --environment applies only to PostgreSQL bindings.
Application output is streamed; --output is not supported. Stop with Ctrl+C.
Credentials are not saved or printed by the launcher; trusted application code
can still log the environment it receives.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("output") {
				return errors.New("--output cannot be used with spring run; application output is streamed")
			}
			directory, err := os.Getwd()
			if err != nil {
				return err
			}
			return spring.Run(cmd.Context(), spring.RunOptions{
				Directory: directory, Environment: extCtx.Environment,
				EnvironmentExplicit: cmd.Flags().Changed("environment"),
			}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}
