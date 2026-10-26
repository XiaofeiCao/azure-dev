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
read the selected provisioned azd environment's database outputs and password.
The database firewall must separately allow your developer IP. Local and deployed
applications in that environment share database data.

This command executes the project's Maven wrapper or Maven on PATH. It does not
provision, deploy, or change firewall rules. Both generated and application-owned
Dockerfiles are supported; local execution uses Maven, not Docker.
MySQL, managed identity, and extra Maven/application arguments are not supported.
--environment requires a non-empty name and applies only to PostgreSQL bindings.
Saved profiles and datasource bindings are used; inherited conflicting overrides
are rejected.
PostgreSQL verify-ca/verify-full currently require the generated Linux CA bundle.
Application output is streamed; --output is not supported. Stop with Ctrl+C.
The provisioning hook stores the password in azd environment state. This launcher
does not save or print it; trusted application code can still log its environment.`,
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
