// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package main

import (
	"os"

	"github.com/azure/azure-dev/cli/azd/extensions/azure.springboot/internal/cmd"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

func main() {
	root := cmd.NewRootCommand(os.Stdout, os.Stderr)
	azdext.Run(root)
}
