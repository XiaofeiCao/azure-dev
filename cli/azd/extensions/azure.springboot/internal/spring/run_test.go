// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package spring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/test/mocks/mockexec"
)

const runDatabaseID = "/subscriptions/test-sub/resourceGroups/test-rg/providers/" +
	"Microsoft.DBforPostgreSQL/flexibleServers/test-server/databases/inventory_db"

func runFixture(t *testing.T, kind ResourceKind) string {
	t.Helper()
	a := &Application{Name: "inventory", JavaVersion: 17, JarName: "inventory.jar",
		Port: 9090, Profiles: []string{"release"}}
	if kind != "" {
		options, err := jdbcOptions(kind, "connectTimeout=10")
		require.NoError(t, err)
		a.Requirements = []Requirement{{
			Kind: kind, Name: "inventory_db", Authentication: "password", JDBCOptions: options,
		}}
	}
	plan, err := Generate(t.Context(), a)
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, plan.Write(t.Context(), directory))
	writeFixture(t, directory, "pom.xml", "<project><packaging>jar</packaging></project>")
	return directory
}

func noPostgres(t *testing.T) func(context.Context, string, string) (map[string]string, error) {
	t.Helper()
	return func(context.Context, string, string) (map[string]string, error) {
		t.Fatal("local-only execution must not access azd environments or Azure")
		return nil, nil
	}
}

func TestLoadRunProject(t *testing.T) {
	for _, kind := range []ResourceKind{"", PostgreSQL, MySQL} {
		t.Run(string(kind), func(t *testing.T) {
			directory := runFixture(t, kind)
			p, err := loadRunProject(t.Context(), directory)
			if kind == MySQL {
				require.ErrorContains(t, err, "only embedded")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "release", p.profiles)
			require.Equal(t, "9090", p.port)
			if kind == PostgreSQL {
				require.Equal(t, "inventory_db", p.database)
			} else {
				require.Empty(t, p.database)
			}
		})
	}
}

func TestLoadRunProjectRejectsModifiedContract(t *testing.T) {
	for _, tt := range []struct {
		name, file, from, to, message string
	}{
		{"missing yaml", "azure.yaml", "", "", "azure.yaml is required"},
		{"missing properties", ".azd-spring/application-azd.properties", "", "", "properties file is missing"},
		{"missing pom", "pom.xml", "", "", "pom.xml is required"},
		{"multi-module", "pom.xml", "<packaging>jar</packaging>",
			"<modules><module>child</module></modules>", "single-module"},
		{"service path", "azure.yaml", "project: .", "project: child", "root Maven service"},
		{"wrong host", "azure.yaml", "host: containerapp", "host: appservice", "root Maven service"},
		{"different host environment", "azure.yaml", "file:/app/config/application-azd.properties",
			"file:/other/application.properties", "host environment differs"},
		{"multiple services", "azure.yaml", "services:",
			"services:\n    extra:\n        project: .\n        host: containerapp\n        language: docker",
			"single service"},
		{"extra dependency", "azure.yaml", "uses:\n", "uses:\n      - unknown\n", "cannot parse azure.yaml"},
		{"modified password", ".azd-spring/application-azd.properties", "${POSTGRES_PASSWORD}",
			"must-not-be-used", "do not match"},
		{"unsafe jdbc option", ".azd-spring/application-azd.properties", "connectTimeout=10",
			"password=must-not-be-used", "do not match"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory := runFixture(t, PostgreSQL)
			file := filepath.Join(directory, filepath.FromSlash(tt.file))
			if tt.from == "" {
				require.NoError(t, os.Remove(file))
			} else {
				data, err := os.ReadFile(file) //nolint:gosec // Fixed fixture paths under t.TempDir.
				require.NoError(t, err)
				require.Contains(t, string(data), tt.from)
				replaced := []byte(strings.ReplaceAll(string(data), tt.from, tt.to))
				require.NoError(t, os.WriteFile(file, replaced, 0600)) //nolint:gosec // Fixed fixture path.
			}
			_, err := loadRunProject(t.Context(), directory)
			require.ErrorContains(t, err, tt.message)
			require.NotContains(t, err.Error(), "must-not-be-used")
		})
	}
}

func TestRunEnvironmentAndCommand(t *testing.T) {
	for _, kind := range []ResourceKind{"", PostgreSQL} {
		t.Run(string(kind), func(t *testing.T) {
			directory := runFixture(t, kind)
			parent := []string{"UNRELATED=keep", "POSTGRES_PASSWORD=stale", "AZURE_SECRET=never-forward-dotenv"}
			before := slices.Clone(parent)
			runner := mockexec.NewMockCommandRunner()
			runner.MockToolInPath("mvn", nil)
			calls := 0
			runner.When(func(args exec.RunArgs, _ string) bool { return args.Cmd == "mvn" }).
				RespondFn(func(args exec.RunArgs) (exec.RunResult, error) {
					calls++
					require.Equal(t, []string{"spring-boot:run"}, args.Args)
					require.Equal(t, directory, args.Cwd)
					require.True(t, args.Interactive)
					require.NotNil(t, args.DebugLogging)
					require.False(t, *args.DebugLogging)
					env := map[string]string{}
					for _, entry := range args.Env {
						key, value, _ := strings.Cut(entry, "=")
						env[key] = value
					}
					require.Equal(t, "keep", env["UNRELATED"])
					require.Equal(t, "release", env["SPRING_PROFILES_ACTIVE"])
					require.Equal(t, "9090", env["SERVER_PORT"])
					expected := fileURI(filepath.Join(directory, ".azd-spring", "application-azd.properties"), runtime.GOOS)
					require.Equal(t, expected, env["SPRING_CONFIG_ADDITIONAL_LOCATION"])
					require.NotContains(t, strings.Join(args.Args, " "), "runtime-password")
					if kind == PostgreSQL {
						require.Equal(t, "runtime-password", env["POSTGRES_PASSWORD"])
						require.Equal(t, "server.postgres.database.azure.com", env["POSTGRES_HOST"])
						require.Equal(t, "5432", env["POSTGRES_PORT"])
						require.Equal(t, "inventory_db", env["POSTGRES_DATABASE"])
						require.Equal(t, "admin", env["POSTGRES_USERNAME"])
						require.NotContains(t, env, "DOTENV_ONLY")
					} else {
						require.NotContains(t, env, "POSTGRES_HOST")
					}
					return exec.RunResult{}, nil
				})
			resolve := noPostgres(t)
			if kind == PostgreSQL {
				resolve = func(_ context.Context, environment, database string) (map[string]string, error) {
					require.Equal(t, "selected", environment)
					require.Equal(t, "inventory_db", database)
					return map[string]string{
						"POSTGRES_HOST": "server.postgres.database.azure.com", "POSTGRES_PORT": "5432",
						"POSTGRES_DATABASE": "inventory_db", "POSTGRES_USERNAME": "admin",
						"POSTGRES_PASSWORD": "runtime-password",
					}, nil
				}
			}
			require.NoError(t, run(t.Context(), RunOptions{Directory: directory, Environment: "selected"},
				runner, parent, resolve))
			require.Equal(t, 1, calls)
			require.Equal(t, before, parent)
			require.NoError(t, filepath.WalkDir(directory, func(file string, entry os.DirEntry, err error) error {
				require.NoError(t, err)
				if !entry.IsDir() {
					data, err := os.ReadFile(file) //nolint:gosec // Walking only this test's generated files.
					require.NoError(t, err)
					require.NotContains(t, string(data), "runtime-password")
				}
				return nil
			}))
		})
	}
}

func TestRunRejectsIgnoredEnvironment(t *testing.T) {
	directory := runFixture(t, "")
	err := run(t.Context(), RunOptions{Directory: directory, Environment: "selected", EnvironmentExplicit: true},
		mockexec.NewMockCommandRunner(), nil, noPostgres(t))
	require.ErrorContains(t, err, "--environment")
}

func TestRunOverrides(t *testing.T) {
	directory := runFixture(t, PostgreSQL)
	p, err := loadRunProject(t.Context(), directory)
	require.NoError(t, err)
	for _, tt := range []struct {
		name, entry, message string
	}{
		{"datasource", "SPRING_DATASOURCE_URL=jdbc:postgresql://user:private@host/db?sig=hidden#fragment",
			"spring.datasource.url"},
		{"username", "SPRING_DATASOURCE_USERNAME=private", "spring.datasource.username"},
		{"password", "SPRING_DATASOURCE_PASSWORD=private", "spring.datasource.password"},
		{"profile", "SPRING_PROFILES_ACTIVE=private", "spring.profiles"},
		{"profile include", "SPRING_PROFILES_INCLUDE=private", "spring.profiles"},
		{"port", "SERVER_PORT=8080", "server.port"},
		{"location", "SPRING_CONFIG_ADDITIONAL_LOCATION=https://user:private@host/?sig=hidden", "spring.config"},
		{"hikari", "SPRING_DATASOURCE_HIKARI_PASSWORD=private", "spring.datasource.hikari.password"},
		{"custom pool", "SPRING_DATASOURCE_TYPE=private", "spring.datasource.type"},
		{"tomcat", "SPRING_DATASOURCE_TOMCAT_URL=private", "spring.datasource.tomcat.url"},
		{"dbcp", "SPRING_DATASOURCE_DBCP2_PASSWORD=private", "spring.datasource.dbcp2.password"},
		{"hikari map", "SPRING_DATASOURCE_HIKARI_DATASOURCEPROPERTIES_USER=private", "Hikari"},
		{"json url", `SPRING_APPLICATION_JSON={"spring":{"datasource":{"url":"private"}}}`, "spring.datasource.url"},
		{"json profile", `SPRING_APPLICATION_JSON={"spring.profiles.active":"private"}`, "spring.profiles"},
		{"json pool", `SPRING_APPLICATION_JSON={"spring.datasource.hikari.password":"private"}`,
			"spring.datasource.hikari.password"},
		{"json invalid", `SPRING_APPLICATION_JSON={"private"`, "valid JSON"},
		{"java", "JAVA_TOOL_OPTIONS=-Dspring.datasource.password=private", "JAVA_TOOL_OPTIONS"},
		{"maven", "MAVEN_OPTS=-Dspring-boot.run.arguments=private", "MAVEN_OPTS"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRunOverrides(t.Context(), directory, p, []string{tt.entry})
			require.ErrorContains(t, err, tt.message)
			for _, sensitive := range []string{"private", "sig=hidden", "fragment", "user:"} {
				require.NotContains(t, err.Error(), sensitive)
			}
		})
	}
	require.NoError(t, checkRunOverrides(t.Context(), directory, p,
		[]string{"SPRING_PROFILES_ACTIVE=release", "SERVER_PORT=9090", "UNRELATED=keep",
			"SPRING_DATASOURCE_TYPE=com.zaxxer.hikari.HikariDataSource"}))
	// JSON wins over an ordinary environment variable, including for conflict detection.
	err = checkRunOverrides(t.Context(), directory, p, []string{
		"SERVER_PORT=9090", `SPRING_APPLICATION_JSON={"server.port":"8080"}`,
	})
	require.ErrorContains(t, err, "server.port")
	writeFixture(t, directory, "src/main/resources/application-release.properties",
		"spring.datasource.hikari.username=private")
	require.ErrorContains(t, checkRunOverrides(t.Context(), directory, p, nil), "spring.datasource.hikari.username")
}

func TestRunPreservesProfileGroups(t *testing.T) {
	directory := runFixture(t, "")
	p, err := loadRunProject(t.Context(), directory)
	require.NoError(t, err)
	p.profiles = "release,sql"
	writeFixture(t, directory, "src/main/resources/application.properties", "spring.profiles.group.release=sql")
	require.NoError(t, checkRunOverrides(t.Context(), directory, p, []string{"SPRING_PROFILES_ACTIVE=release"}))
}

type runEnvironmentClient struct {
	azdext.EnvironmentServiceClient
	mock.Mock
}

func (c *runEnvironmentClient) GetCurrent(
	ctx context.Context, request *azdext.EmptyRequest, _ ...grpc.CallOption,
) (*azdext.EnvironmentResponse, error) {
	args := c.Called(ctx, request)
	response, _ := args.Get(0).(*azdext.EnvironmentResponse)
	return response, args.Error(1)
}

func (c *runEnvironmentClient) GetValues(
	ctx context.Context, request *azdext.GetEnvironmentRequest, _ ...grpc.CallOption,
) (*azdext.KeyValueListResponse, error) {
	args := c.Called(ctx, request)
	response, _ := args.Get(0).(*azdext.KeyValueListResponse)
	return response, args.Error(1)
}

type runAccountClient struct {
	azdext.AccountServiceClient
	mock.Mock
}

func (c *runAccountClient) LookupTenant(
	ctx context.Context, request *azdext.LookupTenantRequest, _ ...grpc.CallOption,
) (*azdext.LookupTenantResponse, error) {
	args := c.Called(ctx, request)
	response, _ := args.Get(0).(*azdext.LookupTenantResponse)
	return response, args.Error(1)
}

func postgresOutputs() map[string]string {
	//nolint:gosec // Deliberately stale test credentials, never used for authentication.
	return map[string]string{
		"AZURE_RESOURCE_INVENTORY_DB_ID": runDatabaseID,
		"AZURE_RESOURCE_APP_ID": "/subscriptions/test-sub/resourceGroups/test-rg/providers/" +
			"Microsoft.App/containerApps/app",
		"AZURE_SUBSCRIPTION_ID":    "test-sub",
		"AZURE_KEY_VAULT_ENDPOINT": "https://test-vault.vault.azure.net/",
		"AZURE_TENANT_ID":          "resource-tenant-not-for-guest-access",
		"POSTGRES_PASSWORD":        "stale-dotenv-password",
	}
}

func environmentValues(values map[string]string) *azdext.KeyValueListResponse {
	response := &azdext.KeyValueListResponse{}
	for key, value := range values {
		response.KeyValues = append(response.KeyValues, &azdext.KeyValue{Key: key, Value: value})
	}
	return response
}

func TestPostgresEnvironmentSelection(t *testing.T) {
	for _, environment := range []string{"", "other"} {
		t.Run("environment="+environment, func(t *testing.T) {
			environments, account := &runEnvironmentClient{}, &runAccountClient{}
			selected := environment
			if selected == "" {
				selected = "current"
				environments.On("GetCurrent", mock.Anything, &azdext.EmptyRequest{}).
					Return(&azdext.EnvironmentResponse{Environment: &azdext.Environment{Name: selected}}, nil).Once()
			}
			values := postgresOutputs()
			if selected == "other" {
				values["AZURE_RESOURCE_INVENTORY_DB_ID"] = strings.ReplaceAll(runDatabaseID, "test-server", "other-server")
			}
			values["AZURE_RESOURCE_ARBITRARY_NORMALIZED_ID"] = values["AZURE_RESOURCE_INVENTORY_DB_ID"]
			delete(values, "AZURE_RESOURCE_INVENTORY_DB_ID")
			environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: selected}).
				Return(environmentValues(values), nil).Once()
			account.On("LookupTenant", mock.Anything, &azdext.LookupTenantRequest{SubscriptionId: "test-sub"}).
				Return(&azdext.LookupTenantResponse{TenantId: "user-access-tenant"}, nil).Once()
			called := false
			result, err := resolvePostgresEnvironment(t.Context(), environment, "inventory_db", environments, account,
				func(_ context.Context, tenant string, id *arm.ResourceID, vault string) (map[string]string, error) {
					called = true
					require.Equal(t, "user-access-tenant", tenant)
					require.Equal(t, values["AZURE_RESOURCE_ARBITRARY_NORMALIZED_ID"], id.String())
					require.Equal(t, "https://test-vault.vault.azure.net", vault)
					return map[string]string{"POSTGRES_PASSWORD": "resolved-only"}, nil
				})
			require.NoError(t, err)
			require.True(t, called)
			require.Equal(t, map[string]string{"POSTGRES_PASSWORD": "resolved-only"}, result)
			environments.AssertExpectations(t)
			account.AssertExpectations(t)
		})
	}
}

func TestPostgresMissingCurrentEnvironment(t *testing.T) {
	environments := &runEnvironmentClient{}
	environments.On("GetCurrent", mock.Anything, &azdext.EmptyRequest{}).
		Return(nil, errors.New("https://user:private@host/?sig=hidden#fragment")).Once()
	_, err := resolvePostgresEnvironment(t.Context(), "", "inventory_db", environments, nil, nil)
	local, ok := errors.AsType[*azdext.LocalError](err)
	require.True(t, ok)
	require.Contains(t, local.Suggestion, "azd provision")
	require.NotContains(t, err.Error(), "private")
	environments.AssertExpectations(t)
}

func TestPostgresTenantFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response *azdext.LookupTenantResponse
		err      error
	}{
		{"no tenant", &azdext.LookupTenantResponse{}, nil},
		{"lookup denied", nil, errors.New("https://user:private@host/?sig=hidden")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environments, account := &runEnvironmentClient{}, &runAccountClient{}
			environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: "selected"}).
				Return(environmentValues(postgresOutputs()), nil).Once()
			account.On("LookupTenant", mock.Anything, &azdext.LookupTenantRequest{SubscriptionId: "test-sub"}).
				Return(tt.response, tt.err).Once()
			_, err := resolvePostgresEnvironment(t.Context(), "selected", "inventory_db", environments, account, nil)
			require.ErrorContains(t, err, "user-access tenant")
			require.NotContains(t, err.Error(), "private")
			environments.AssertExpectations(t)
			account.AssertExpectations(t)
		})
	}
}

func TestPostgresRejectsBadOutputs(t *testing.T) {
	for _, tt := range []struct {
		name, key, value, message string
	}{
		{"missing database", "AZURE_RESOURCE_INVENTORY_DB_ID", "", "no matching PostgreSQL"},
		{"malformed id", "AZURE_RESOURCE_INVENTORY_DB_ID", "https://user:private@host/?sig=hidden",
			"no matching PostgreSQL"},
		{"wrong type", "AZURE_RESOURCE_INVENTORY_DB_ID",
			strings.ReplaceAll(runDatabaseID, "Microsoft.DBforPostgreSQL", "Microsoft.DBforMySQL"),
			"no matching PostgreSQL"},
		{"subscription", "AZURE_SUBSCRIPTION_ID", "another-sub", "subscription"},
		{"ambiguous", "AZURE_RESOURCE_OTHER_ID",
			strings.ReplaceAll(runDatabaseID, "test-server", "other-server"), "multiple provisioned"},
		{"no vault", "AZURE_KEY_VAULT_ENDPOINT", "", "invalid AZURE_KEY_VAULT_ENDPOINT"},
		{"vault userinfo", "AZURE_KEY_VAULT_ENDPOINT", "https://user:private@test.vault.azure.net/", "invalid"},
		{"vault query", "AZURE_KEY_VAULT_ENDPOINT", "https://test.vault.azure.net/?sig=hidden#fragment", "invalid"},
		{"vault host", "AZURE_KEY_VAULT_ENDPOINT", "https://test.vault.azure.net.attacker.example/", "invalid"},
		{"vault http", "AZURE_KEY_VAULT_ENDPOINT", "http://test.vault.azure.net/", "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := postgresOutputs()
			values[tt.key] = tt.value
			environments := &runEnvironmentClient{}
			environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: "selected"}).
				Return(environmentValues(values), nil).Once()
			_, err := resolvePostgresEnvironment(t.Context(), "selected", "inventory_db", environments, nil, nil)
			require.ErrorContains(t, err, tt.message)
			for _, sensitive := range []string{"private", "sig=hidden", "fragment"} {
				require.NotContains(t, err.Error(), sensitive)
			}
			environments.AssertExpectations(t)
		})
	}
}

func TestPostgresConnection(t *testing.T) {
	id, err := arm.ParseResourceID(runDatabaseID)
	require.NoError(t, err)
	properties := map[string]any{
		"fullyQualifiedDomainName": "test-server.postgres.database.azure.com", "administratorLogin": "actual-admin",
	}
	for _, tt := range []struct {
		name, missing, password, message string
		serviceErr                       error
		secretErr                        error
	}{
		{name: "success", password: "runtime-password"},
		{name: "no host", missing: "fullyQualifiedDomainName", message: "no valid hostname"},
		{name: "no admin", missing: "administratorLogin", message: "no valid hostname"},
		{name: "empty secret", message: "secret is empty"},
		{name: "server denied", serviceErr: &azcore.ResponseError{StatusCode: 403}, message: "HTTP 403"},
		{name: "secret denied", secretErr: &azcore.ResponseError{StatusCode: 403}, message: "HTTP 403"},
		{name: "secret missing", secretErr: &azcore.ResponseError{StatusCode: 404}, message: "HTTP 404"},
		{name: "unsafe error", secretErr: errors.New("https://user:private@host/?sig=hidden#fragment"),
			message: "Key Vault"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			props := maps.Clone(properties)
			delete(props, tt.missing)
			result, err := readPostgresConnection(t.Context(), id, "https://vault.vault.azure.net",
				func(_ context.Context, server string) (any, error) {
					require.Equal(t, id.Parent.String(), server)
					return props, tt.serviceErr
				}, func(_ context.Context, reference string) (string, error) {
					require.Equal(t,
						"@Microsoft.KeyVault(SecretUri=https://vault.vault.azure.net/secrets/postgres-password)", reference)
					return tt.password, tt.secretErr
				})
			if tt.message != "" {
				require.ErrorContains(t, err, tt.message)
				require.Nil(t, result)
				for _, sensitive := range []string{"private", "sig=hidden", "fragment", "runtime-password"} {
					require.NotContains(t, err.Error(), sensitive)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, map[string]string{
				"POSTGRES_HOST": "test-server.postgres.database.azure.com", "POSTGRES_PORT": "5432",
				"POSTGRES_DATABASE": "inventory_db", "POSTGRES_USERNAME": "actual-admin",
				"POSTGRES_PASSWORD": "runtime-password",
			}, result)
		})
	}
}

func TestMavenWrapperAndFileURI(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			directory := t.TempDir()
			runner := mockexec.NewMockCommandRunner()
			runner.MockToolInPath("mvn", nil)
			command, err := mavenCommand(directory, goos, runner)
			require.NoError(t, err)
			require.Equal(t, "mvn", command)
			writeFixture(t, directory, "mvnw", "")
			writeFixture(t, directory, "mvnw.cmd", "")
			command, err = mavenCommand(directory, goos, runner)
			require.NoError(t, err)
			expected := "mvnw"
			if goos == "windows" {
				expected = "mvnw.cmd"
			}
			require.Equal(t, filepath.Join(directory, expected), command)
		})
	}
	require.Equal(t, "file:///C:/my%20app/%23data/application.properties",
		fileURI(`C:\my app\#data\application.properties`, "windows"))
	require.Equal(t, "file://server/share/my%20app/application.properties",
		fileURI(`\\server\share\my app\application.properties`, "windows"))
	require.Equal(t, "file:///my%20app/%23data/application.properties",
		fileURI("/my app/#data/application.properties", "linux"))
}

func TestPostgresInvalidServerMetadata(t *testing.T) {
	id, err := arm.ParseResourceID(runDatabaseID)
	require.NoError(t, err)
	for _, props := range []any{
		nil, "not an object",
		//nolint:gosec // Deliberately invalid URL tests credential non-disclosure.
		map[string]any{"fullyQualifiedDomainName": "https://user:private@host/?sig=hidden", "administratorLogin": "admin"},
		map[string]any{
			"fullyQualifiedDomainName": "host.postgres.database.azure.com", "administratorLogin": "admin",
			"authConfig": map[string]any{"passwordAuth": "Disabled"},
		},
	} {
		_, err := readPostgresConnection(t.Context(), id, "https://test.vault.azure.net",
			func(context.Context, string) (any, error) { return props, nil },
			func(context.Context, string) (string, error) {
				t.Fatal("invalid server metadata must not trigger a secret lookup")
				return "", nil
			})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private")
	}
}

func TestMergeRunEnvironment(t *testing.T) {
	parent := []string{"Path=keep", "postgres_password=stale", "POSTGRES_PASSWORD=also-stale", "Unrelated=yes"}
	values := map[string]string{"POSTGRES_PASSWORD": "resolved"}
	require.Equal(t, []string{"Path=keep", "POSTGRES_PASSWORD=resolved", "Unrelated=yes"},
		mergeRunEnvironment(parent, values, "windows"))
	require.Equal(t, []string{"POSTGRES_PASSWORD=resolved", "Path=keep", "Unrelated=yes", "postgres_password=stale"},
		mergeRunEnvironment(parent, values, "linux"))
	require.Equal(t, []string{`=C:=C:\workspace`, `=D:=D:\other`},
		mergeRunEnvironment([]string{`=C:=C:\workspace`, `=D:=D:\other`}, nil, "windows"))
}

func TestRunFailureAndCancellation(t *testing.T) {
	directory := runFixture(t, "")
	for _, tt := range []struct {
		name, message string
		failure       error
		cancel        bool
	}{
		{"startup", "starting Maven", errors.New("https://user:private@host/?sig=hidden"), false},
		{"nonzero", "exit code 42", &exec.ExitError{ExitCode: 42}, false},
		{"cancel", "context canceled", errors.New("killed"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runner := mockexec.NewMockCommandRunner()
			runner.MockToolInPath("mvn", nil)
			runner.When(func(exec.RunArgs, string) bool { return true }).
				RespondFn(func(exec.RunArgs) (exec.RunResult, error) {
					if tt.cancel {
						cancel()
					}
					return exec.RunResult{}, tt.failure
				})
			err := run(ctx, RunOptions{Directory: directory}, runner, nil, noPostgres(t))
			require.ErrorContains(t, err, tt.message)
			require.NotContains(t, err.Error(), "private")
			if tt.cancel {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestRunStreamsWithoutDebugDisclosure(t *testing.T) {
	base := runFixture(t, PostgreSQL)
	directory := filepath.Join(t.TempDir(), "application with spaces")
	require.NoError(t, os.Mkdir(directory, 0700))
	require.NoError(t, os.CopyFS(directory, os.DirFS(base)))
	wrapper, script := "mvnw", "#!/bin/sh\nread input\n"+
		`test "$1" = "spring-boot:run" || exit 9`+"\n"+
		`test -n "$POSTGRES_PASSWORD" || exit 10`+"\n"+
		`echo "APP:$SPRING_PROFILES_ACTIVE:$SERVER_PORT:$UNRELATED:$input"`+"\n"+
		"echo application-warning >&2\n"
	if runtime.GOOS == "windows" {
		wrapper = "mvnw.cmd"
		script = "@echo off\r\nif not \"%~1\"==\"spring-boot:run\" exit /b 9\r\n" +
			"if not defined POSTGRES_PASSWORD exit /b 10\r\nset /p INPUT=\r\n" +
			"echo APP:%SPRING_PROFILES_ACTIVE%:%SERVER_PORT%:%UNRELATED%:%INPUT%\r\n" +
			"echo application-warning 1>&2\r\n"
	}
	file := filepath.Join(directory, wrapper)
	require.NoError(t, os.WriteFile(file, []byte(script), 0600))
	require.NoError(t, os.Chmod(file, 0700)) //nolint:gosec // Execute this test's Maven wrapper.
	var output, diagnostic, logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	runner := newRunRunner(strings.NewReader("input-value\n"), &output, &diagnostic)
	resolve := func(context.Context, string, string) (map[string]string, error) {
		return map[string]string{"POSTGRES_PASSWORD": "non-disclosure-password"}, nil
	}
	require.NoError(t, run(t.Context(), RunOptions{Directory: directory}, runner, []string{"UNRELATED=keep"}, resolve))
	require.Contains(t, output.String(), "APP:release:9090:keep:input-value")
	require.Contains(t, diagnostic.String(), "application-warning")
	require.NotContains(t, output.String()+diagnostic.String()+logs.String(), "non-disclosure-password")
	require.NotContains(t, logs.String(), "Additional env")
}

type runReadyWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	once   sync.Once
	ready  chan struct{}
}

func (w *runReadyWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(data)
	if strings.Contains(w.buffer.String(), "READY") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestRunCancelsProcessTree(t *testing.T) {
	directory := runFixture(t, "")
	pidFile := filepath.Join(directory, "child.pid")
	wrapper := "mvnw"
	script := "#!/bin/sh\nsleep 60 &\necho $! > \"$SPRING_RUN_TEST_PID\"\necho READY\nwait\n"
	if runtime.GOOS == "windows" {
		wrapper = "mvnw.cmd"
		script = "@echo off\r\npowershell.exe -NoProfile -Command " +
			`"$PID | Set-Content -LiteralPath $env:SPRING_RUN_TEST_PID; Write-Output 'READY'; ` +
			`while ($true) { Start-Sleep -Seconds 1 }"` + "\r\n"
	}
	file := filepath.Join(directory, wrapper)
	require.NoError(t, os.WriteFile(file, []byte(script), 0600))
	require.NoError(t, os.Chmod(file, 0700)) //nolint:gosec // Execute this test's Maven wrapper.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &runReadyWriter{ready: make(chan struct{})}
	runner := newRunRunner(strings.NewReader(""), output, output)
	done := make(chan error, 1)
	parent := mergeRunEnvironment(os.Environ(), map[string]string{"SPRING_RUN_TEST_PID": pidFile}, runtime.GOOS)
	go func() {
		done <- run(ctx, RunOptions{Directory: directory}, runner, parent, noPostgres(t))
	}()
	select {
	case <-output.ready:
	case err := <-done:
		t.Fatalf("application exited before becoming ready: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("application did not become ready")
	}
	data, err := os.ReadFile(pidFile) //nolint:gosec // This test's child PID file under t.TempDir.
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Positive(t, pid)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		process, err := os.FindProcess(pid)
		require.NoError(t, err)
		require.NoError(t, process.Kill())
		t.Fatal("cancellation did not wait for process-tree cleanup")
	}
	if runtime.GOOS == "windows" {
		check := fmt.Sprintf("if (Get-Process -Id %d -ErrorAction SilentlyContinue) { exit 1 }", pid)
		//nolint:gosec // Inspect only the validated PID created by this test.
		require.NoError(t, osexec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-Command", check).Run())
	} else {
		//nolint:gosec // Inspect only the validated PID created by this test.
		state, err := osexec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		// A killed descendant may briefly remain as a zombie until its new parent reaps it.
		require.True(t, err != nil || strings.HasPrefix(strings.TrimSpace(string(state)), "Z"), string(state))
	}
}

func TestRunEmptyProfilesAndEmbeddedBinding(t *testing.T) {
	directory := runFixture(t, "")
	p, err := loadRunProject(t.Context(), directory)
	require.NoError(t, err)
	p.profiles = ""
	require.NoError(t, checkRunOverrides(t.Context(), directory, p, nil))
	require.ErrorContains(t, checkRunOverrides(t.Context(), directory, p,
		[]string{"SPRING_PROFILES_DEFAULT=release"}), "spring.profiles.default")
	require.NoError(t, checkRunOverrides(t.Context(), directory, p,
		[]string{"SPRING_DATASOURCE_URL=jdbc:h2:mem:local"}))
	require.ErrorContains(t, checkRunOverrides(t.Context(), directory, p,
		[]string{"SPRING_DATASOURCE_URL=jdbc:postgresql://user:private@host/db"}), "local-only binding")
	require.ErrorContains(t, checkRunOverrides(t.Context(), directory, p,
		[]string{"SPRING_DATASOURCE_HIKARI_JDBCURL=jdbc:postgresql://user:private@host/db"}), "hikari.jdbc-url")
}

func TestRunMissingMaven(t *testing.T) {
	runner := mockexec.NewMockCommandRunner()
	runner.MockToolInPath("mvn", errors.New("not on PATH"))
	_, err := mavenCommand(t.TempDir(), runtime.GOOS, runner)
	require.ErrorContains(t, err, "Maven is not available")
}

func TestRunNativeFailure(t *testing.T) {
	directory := runFixture(t, "")
	wrapper, script := "mvnw", "#!/bin/sh\nexit 37\n"
	if runtime.GOOS == "windows" {
		wrapper, script = "mvnw.cmd", "@exit /b 37\r\n"
	}
	file := filepath.Join(directory, wrapper)
	require.NoError(t, os.WriteFile(file, []byte(script), 0600))
	require.NoError(t, os.Chmod(file, 0700)) //nolint:gosec // Execute this test's Maven wrapper.
	var output bytes.Buffer
	err := run(t.Context(), RunOptions{Directory: directory},
		newRunRunner(strings.NewReader(""), &output, &output), os.Environ(), noPostgres(t))
	require.ErrorContains(t, err, "exit code 37")
	failure, ok := errors.AsType[*azdext.ToolError](err)
	require.True(t, ok)
	require.Equal(t, 37, *failure.ExitCode)
}

func TestRunErrorIsSafe(t *testing.T) {
	for _, err := range []error{
		nil, errors.New("https://user:private@host/?sig=hidden#fragment"),
		fmt.Errorf("request failed: %w", &azcore.ResponseError{StatusCode: 403}),
	} {
		safe := runError("reading credentials", err, "Check access.")
		require.ErrorContains(t, safe, "reading credentials failed")
		require.NotContains(t, safe.Error(), "private")
	}
	require.ErrorIs(t, runError("cancelled", context.Canceled, ""), context.Canceled)
	require.ErrorIs(t, runError("expired", context.DeadlineExceeded, ""), context.DeadlineExceeded)
}
