// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package spring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/test/mocks/mockexec"
)

func runFixture(t *testing.T, kind ResourceKind) string {
	t.Helper()
	a := &Application{Name: "inventory", JavaVersion: 17, JarName: "inventory.jar",
		Port: 9090, Profiles: []string{"release"}, BuildDirectory: "target"}
	if kind != "" {
		options, err := jdbcOptions(kind, "connectTimeout=10")
		require.NoError(t, err)
		a.Requirements = []Requirement{{
			Kind: kind, Name: "inventory_db", Authentication: "password", JDBCOptions: options,
		}}
	}
	return writeRunFixture(t, a)
}

func writeRunFixture(t *testing.T, a *Application) string {
	t.Helper()
	plan, err := Generate(t.Context(), a)
	require.NoError(t, err)
	directory := t.TempDir()
	if a.Dockerfile != "" {
		writeFixture(t, directory, a.Dockerfile, "FROM scratch\n")
	}
	require.NoError(t, plan.Write(t.Context(), directory))
	writeFixture(t, directory, "pom.xml", "<project><packaging>jar</packaging></project>")
	return directory
}

func setRunParameter(t *testing.T, directory, file, key string, value any) {
	t.Helper()
	name := filepath.Join(directory, "infra", file+".parameters.json")
	data, err := os.ReadFile(name) //nolint:gosec // Fixed test fixture parameter files.
	require.NoError(t, err)
	var document struct {
		Parameters map[string]map[string]any `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal(data, &document))
	if value == nil {
		delete(document.Parameters, key)
	} else {
		document.Parameters[key] = map[string]any{"value": value}
	}
	data, err = json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, data, 0600))
}

func TestRunSavedParameters(t *testing.T) {
	for _, tt := range []struct {
		name, key, message string
		value              any
	}{
		{"missing", "port", "missing or null", nil},
		{"null", "port", "missing or null", json.RawMessage("null")},
		{"wrong type", "port", "parameter types", "9090"},
		{"out of range", "port", "parameter types", 70000},
		{"wrong profile", "profiles", "profile selection", []string{"not a profile"}},
		{"disagreement", "port", "parameters disagree", 8080},
		{"mysql", "engine", "only embedded", "mysql"},
		{"managed identity", "authentication", "password authentication", "managedIdentity"},
		{"literal password", "password", "parameter binding", "private"},
		{"literal host", "host", "parameter binding", "user:private@host?sig=hidden"},
		{"wrong username", "username", "parameter binding", "other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory := runFixture(t, PostgreSQL)
			setRunParameter(t, directory, "application", tt.key, tt.value)
			_, err := loadRunProject(t.Context(), directory)
			require.ErrorContains(t, err, tt.message)
			require.NotContains(t, err.Error(), "private")
			require.NotContains(t, err.Error(), "sig=hidden")
		})
	}
	for _, file := range []string{"main", "application"} {
		for _, contents := range []string{"", "{}", `{"parameters":{"port":9090}}`} {
			directory := runFixture(t, "")
			writeFixture(t, directory, "infra/"+file+".parameters.json", contents)
			_, err := loadRunProject(t.Context(), directory)
			require.Error(t, err)
		}
	}
	directory := runFixture(t, "")
	require.NoError(t, os.Remove(filepath.Join(directory, "infra", "main.parameters.json")))
	_, err := loadRunProject(t.Context(), directory)
	require.ErrorContains(t, err, "fresh project")
	directory = runFixture(t, "")
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	defer root.Close()
	data, err := root.ReadFile("azure.yaml")
	require.NoError(t, err)
	require.NoError(t, root.WriteFile("azure.yaml", append(data,
		[]byte("\nresources:\n  legacy:\n    type: host.containerapp\n")...), 0600))
	_, err = loadRunProject(t.Context(), directory)
	require.ErrorContains(t, err, "legacy")
}

func TestRunGeneratedBindingModes(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, adopted := range []bool{false, true} {
			t.Run(fmt.Sprintf("postgres=%t/dockerfile=%t", external, adopted), func(t *testing.T) {
				a := &Application{
					Name: "inventory", JavaVersion: 17, JarName: "inventory.jar", BuildDirectory: "target",
					Port: 9090, Profiles: []string{"release"},
					Embedded: &EmbeddedBinding{
						Engine: "h2", URL: "jdbc:h2:mem:catalog", URLConfigured: true, Driver: "org.h2.Driver",
					},
				}
				if external {
					a.Embedded = nil
					options, err := jdbcOptions(PostgreSQL, "sslmode=require")
					require.NoError(t, err)
					a.Requirements = []Requirement{{
						Kind: PostgreSQL, Name: "inventory_db", Authentication: "password",
						JDBCOptions: options, Driver: "org.postgresql.Driver",
					}}
				}
				if adopted {
					a.Dockerfile = "Dockerfile"
				}
				directory := writeRunFixture(t, a)
				if !external {
					writeFixture(t, directory, "src/main/resources/application.properties",
						"spring.datasource.url=${UNRESOLVED_AT_RUNTIME}\n")
				}
				p, err := loadRunProject(t.Context(), directory)
				require.NoError(t, err)
				require.Equal(t, adopted, p.binding.EnvironmentBindings)
				require.NoError(t, checkRunOverrides(t.Context(), directory, p, nil))
				runner := mockexec.NewMockCommandRunner()
				runner.MockToolInPath("mvn", nil)
				runner.When(func(exec.RunArgs, string) bool { return true }).
					RespondFn(func(args exec.RunArgs) (exec.RunResult, error) {
						name := "SPRING_DATASOURCE_URL=jdbc:h2:mem:catalog"
						if external {
							name = "SPRING_DATASOURCE_DRIVERCLASSNAME=org.postgresql.Driver"
						}
						require.Contains(t, args.Env, name)
						return exec.RunResult{}, nil
					})
				resolve := noPostgres(t)
				if external {
					resolve = func(context.Context, string, string) (map[string]string, error) {
						return map[string]string{"POSTGRES_HOST": "host", "POSTGRES_PASSWORD": "secret"}, nil
					}
				}
				require.NoError(t, run(t.Context(), RunOptions{Directory: directory}, runner, nil, resolve))
				if external {
					properties := p.datasourceProperties("host", "secret")
					require.Equal(t, "org.postgresql.Driver", properties["spring.datasource.driver-class-name"])
				} else {
					require.Equal(t, "jdbc:h2:mem:catalog", p.datasourceProperties("", "")["spring.datasource.url"])
					require.ErrorContains(t, checkRunOverrides(t.Context(), directory, p,
						[]string{"SPRING_DATASOURCE_URL=jdbc:h2:mem:other"}), "saved embedded")
				}
			})
		}
	}
}

func TestRunTLS(t *testing.T) {
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		options, err := jdbcOptions(PostgreSQL, "sslmode="+mode)
		require.NoError(t, err)
		require.NoError(t, validateRunJDBCOptions(options))
		if mode != "require" {
			require.ErrorContains(t, checkRunTLS(options, "windows"), "never downgraded")
		} else {
			require.NoError(t, checkRunTLS(options, "windows"))
		}

	}
	for _, options := range []string{
		"sslmode=disable", "sslmode=require&sslrootcert=private", "sslmode=require&password=private",
		"sslmode=%invalid", "sslmode=verify-full&sslrootcert=private", "sslmode=require&sslmode=require",
	} {
		require.Error(t, validateRunJDBCOptions(options))
	}
}

func TestPostgresEnvironmentReadFailure(t *testing.T) {
	environments := &runEnvironmentClient{}
	environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: "selected"}).
		Return(nil, errors.New("https://user:private@host/?sig=hidden#fragment")).Once()
	_, err := resolvePostgresEnvironment(t.Context(), "selected", "inventory_db", environments)
	require.ErrorContains(t, err, "reading the selected azd environment")
	for _, sensitive := range []string{"user:", "private", "sig=hidden", "fragment"} {
		require.NotContains(t, err.Error(), sensitive)
	}
	environments.AssertExpectations(t)
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
		{"different module", "azure.yaml", "module: application", "module: other", "root Maven service"},
		{"multiple services", "azure.yaml", "services:",
			"services:\n    extra:\n        project: .\n        host: containerapp\n        language: docker",
			"single service"},
		{"extra dependency", "azure.yaml", "project: .", "project: .\n        uses: [unknown]", "root Maven service"},
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
					require.Equal(t, "none", env["SPRING_PROFILES_DEFAULT"])
					require.NotContains(t, env, "SPRING_CONFIG_ADDITIONAL_LOCATION")
					require.NotContains(t, strings.Join(args.Args, " "), "runtime-password")
					if kind == PostgreSQL {
						require.Equal(t, "runtime-password", env["POSTGRES_PASSWORD"])
						require.Equal(t, "server.postgres.database.azure.com", env["POSTGRES_HOST"])
						require.Equal(t, "5432", env["POSTGRES_PORT"])
						require.Equal(t, "inventory_db", env["POSTGRES_DATABASE"])
						require.Equal(t, "admin", env["POSTGRES_USERNAME"])
						require.Equal(t, "azdadmin", env["SPRING_DATASOURCE_USERNAME"])
						require.Equal(t, "runtime-password", env["SPRING_DATASOURCE_PASSWORD"])
						require.Equal(t, "jdbc:postgresql://server.postgres.database.azure.com:5432/"+
							"inventory_db?connectTimeout=10&sslmode=require", env["SPRING_DATASOURCE_URL"])
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

func TestRunRejectsEmptyEnvironmentBeforeLoadingProject(t *testing.T) {
	for _, environment := range []string{"", " ", "\t"} {
		err := run(t.Context(), RunOptions{
			Directory: t.TempDir(), Environment: environment, EnvironmentExplicit: true,
		}, mockexec.NewMockCommandRunner(), nil, noPostgres(t))
		require.ErrorContains(t, err, "--environment requires a non-empty")
	}
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
		{"empty driver", "SPRING_DATASOURCE_TOMCAT_DRIVERCLASSNAME=", "spring.datasource.tomcat.driver-class-name"},
		{"hikari map", "SPRING_DATASOURCE_HIKARI_DATASOURCEPROPERTIES_USER=private", "Hikari"},
		{"json url", `SPRING_APPLICATION_JSON={"spring":{"datasource":{"url":"private"}}}`, "spring.datasource.url"},
		{"json profile", `SPRING_APPLICATION_JSON={"spring.profiles.active":"private"}`, "spring.profiles"},
		{"json pool", `SPRING_APPLICATION_JSON={"spring.datasource.hikari.password":"private"}`,
			"spring.datasource.hikari.password"},
		{"json invalid", `SPRING_APPLICATION_JSON={"private"`, "valid JSON"},
		{"java", "JAVA_TOOL_OPTIONS=-Dspring.datasource.password=private", "JAVA_TOOL_OPTIONS"},
		{"java port", "JAVA_TOOL_OPTIONS=-Dserver.port=8080", "JAVA_TOOL_OPTIONS"},
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

func postgresOutputs() map[string]string {
	//nolint:gosec // Deliberately stale test credentials, never used for authentication.
	return map[string]string{
		"SPRING_DATABASE_HOST":        "test-server.postgres.database.azure.com",
		"SPRING_DATABASE_SERVER_NAME": "test-server",
		"SPRING_DATABASE_NAME":        "inventory_db",
		"SPRING_DATABASE_PASSWORD":    "resolved-only",
		"POSTGRES_PASSWORD":           "stale-dotenv-password",
		"UNRELATED_SECRET":            "never-forward",
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
			environments := &runEnvironmentClient{}
			selected := environment
			if selected == "" {
				selected = "current"
				environments.On("GetCurrent", mock.Anything, &azdext.EmptyRequest{}).
					Return(&azdext.EnvironmentResponse{Environment: &azdext.Environment{Name: selected}}, nil).Once()
			}
			values := postgresOutputs()
			if selected == "other" {
				values["SPRING_DATABASE_HOST"] = "other-server.postgres.database.azure.com"
				values["SPRING_DATABASE_SERVER_NAME"] = "other-server"
			}
			environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: selected}).
				Return(environmentValues(values), nil).Once()
			result, err := resolvePostgresEnvironment(t.Context(), environment, "inventory_db", environments)
			require.NoError(t, err)
			require.Equal(t, map[string]string{
				"POSTGRES_HOST": values["SPRING_DATABASE_HOST"], "POSTGRES_PORT": "5432",
				"POSTGRES_DATABASE": "inventory_db", "POSTGRES_USERNAME": "azdadmin", "POSTGRES_PASSWORD": "resolved-only",
			}, result)
			environments.AssertExpectations(t)
		})
	}
}

func TestPostgresMissingCurrentEnvironment(t *testing.T) {
	environments := &runEnvironmentClient{}
	environments.On("GetCurrent", mock.Anything, &azdext.EmptyRequest{}).
		Return(nil, errors.New("https://user:private@host/?sig=hidden#fragment")).Once()
	_, err := resolvePostgresEnvironment(t.Context(), "", "inventory_db", environments)
	local, ok := errors.AsType[*azdext.LocalError](err)
	require.True(t, ok)
	require.Contains(t, local.Suggestion, "azd provision")
	require.NotContains(t, err.Error(), "private")
	environments.AssertExpectations(t)
}

func TestPostgresRejectsBadOutputs(t *testing.T) {
	for _, tt := range []struct {
		name, key, value, message string
	}{
		{"missing database", "SPRING_DATABASE_NAME", "", "no matching PostgreSQL"},
		{"wrong database", "SPRING_DATABASE_NAME", "other_db", "no matching PostgreSQL"},
		{"missing host", "SPRING_DATABASE_HOST", "", "invalid PostgreSQL"},
		{"host userinfo", "SPRING_DATABASE_HOST", "user:private@test.postgres.database.azure.com", "invalid"},
		{"host query", "SPRING_DATABASE_HOST", "test.postgres.database.azure.com/?sig=hidden#fragment", "invalid"},
		{"host suffix", "SPRING_DATABASE_HOST", "test-server.postgres.database.azure.com.attacker.example", "invalid"},
		{"wrong server", "SPRING_DATABASE_SERVER_NAME", "another-server", "invalid"},
		{"no server", "SPRING_DATABASE_SERVER_NAME", "", "invalid"},
		{"no password", "SPRING_DATABASE_PASSWORD", "", "SPRING_DATABASE_PASSWORD"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := postgresOutputs()
			values[tt.key] = tt.value
			environments := &runEnvironmentClient{}
			environments.On("GetValues", mock.Anything, &azdext.GetEnvironmentRequest{Name: "selected"}).
				Return(environmentValues(values), nil).Once()
			_, err := resolvePostgresEnvironment(t.Context(), "selected", "inventory_db", environments)
			require.ErrorContains(t, err, tt.message)
			for _, sensitive := range []string{"private", "sig=hidden", "fragment"} {
				require.NotContains(t, err.Error(), sensitive)
			}
			environments.AssertExpectations(t)
		})
	}
}

func TestMavenWrapper(t *testing.T) {
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
