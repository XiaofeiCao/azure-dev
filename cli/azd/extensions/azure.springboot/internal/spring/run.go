// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package spring

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/magiconair/properties"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
)

// RunOptions selects a generated application and, for cloud bindings, its azd environment.
type RunOptions struct {
	Directory           string
	Environment         string
	EnvironmentExplicit bool
}

type runProject struct {
	database string
	profiles string
	port     string
}

// Run launches a trusted generated Maven application without provisioning or persisting credentials.
func Run(ctx context.Context, options RunOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	if stderr == nil {
		stderr = io.Discard
	}
	previous := log.Writer()
	log.SetOutput(stderr)
	defer log.SetOutput(previous)
	return run(ctx, options, newRunRunner(stdin, stdout, stderr), os.Environ(), resolvePostgres)
}

type unixRunRunner struct {
	exec.CommandRunner
	stdin          io.Reader
	stdout, stderr io.Writer
}

func newRunRunner(stdin io.Reader, stdout, stderr io.Writer) exec.CommandRunner {
	runner := exec.NewCommandRunner(&exec.RunnerOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if runtime.GOOS == "windows" {
		return runner
	}
	return &unixRunRunner{CommandRunner: runner, stdin: stdin, stdout: stdout, stderr: stderr}
}

func (r *unixRunRunner) Run(ctx context.Context, args exec.RunArgs) (exec.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return exec.RunResult{}, err
	}
	// The SDK's interactive Unix runner skips process-group creation but still kills by group.
	// Pipe stdin instead of attaching the terminal to a background process group.
	process := exec.CmdTree{
		Cmd: osexec.Command(args.Cmd, args.Args...), //nolint:gosec // Only the selected Maven command and fixed goal.
	}
	process.Dir, process.Env = args.Cwd, args.Env
	if r.stdin != nil {
		process.Stdin = struct{ io.Reader }{r.stdin}
	}
	process.Stdout, process.Stderr = r.stdout, r.stderr
	process.WaitDelay = 2 * time.Second
	if err := process.Start(); err != nil {
		return exec.RunResult{}, err
	}
	kill := sync.OnceFunc(process.Kill)
	defer kill()
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			kill()
		case <-done:
		}
	}()
	err := process.Wait()
	close(done)
	<-stopped
	result := exec.RunResult{ExitCode: process.ProcessState.ExitCode()}
	if exit, ok := errors.AsType[*osexec.ExitError](err); ok {
		err = &exec.ExitError{Cmd: args.Cmd, ExitCode: exit.ExitCode()}
	}
	return result, err
}

func run(
	ctx context.Context, options RunOptions, runner exec.CommandRunner, parent []string,
	resolve func(context.Context, string, string) (map[string]string, error),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return fmt.Errorf("resolving application directory: %w", err)
	}
	p, err := loadRunProject(ctx, directory)
	if err != nil {
		return err
	}
	if p.database == "" && options.EnvironmentExplicit {
		return errors.New("--environment cannot be used without a cloud database binding; omit --environment")
	}
	values := map[string]string{
		"SPRING_PROFILES_ACTIVE": p.profiles,
		"SERVER_PORT":            p.port,
	}
	if p.profiles == "" {
		values["SPRING_PROFILES_DEFAULT"] = "none"
	}
	if err := checkRunOverrides(ctx, directory, p, parent); err != nil {
		return err
	}
	command, err := mavenCommand(directory, runtime.GOOS, runner)
	if err != nil {
		return err
	}
	if p.database != "" {
		resolved, err := resolve(ctx, options.Environment, p.database)
		if err != nil {
			return err
		}
		maps.Copy(values, resolved)
	}
	values["SPRING_CONFIG_ADDITIONAL_LOCATION"] = fileURI(
		filepath.Join(directory, ".azd-spring", "application-azd.properties"), runtime.GOOS)
	args := exec.NewRunArgs(command, "spring-boot:run").
		WithCwd(directory).
		WithEnv(mergeRunEnvironment(parent, values, runtime.GOOS)).
		WithInteractive(true).
		WithDebugLogging(false)
	// The SDK's debug environment dump is not covered by SensitiveData.
	_, err = runner.Run(ctx, args)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return &azdext.ToolError{
			Message:  fmt.Sprintf("Maven spring-boot:run failed (exit code %d)", exit.ExitCode),
			ToolName: "maven", Kind: azdext.ToolErrorKindFailed, ExitCode: new(exit.ExitCode),
			Suggestion: "Check the application output above.",
		}
	}
	if err != nil {
		return runError("starting Maven", err, "Check Java, Maven, and the project's Maven wrapper.")
	}
	return nil
}

func loadRunProject(ctx context.Context, directory string) (*runProject, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("opening application directory: %w", err)
	}
	defer root.Close()
	data, err := root.ReadFile("azure.yaml")
	if err != nil {
		return nil, errors.New("azure.yaml is required; run azd spring init in the application directory first")
	}
	config, err := project.Parse(ctx, string(data))
	if err != nil {
		return nil, errors.New("cannot parse azure.yaml; restore the project generated by azd spring init")
	}
	if len(config.Services) != 1 {
		return nil, errors.New("spring run requires the single service generated by azd spring init")
	}
	p := &runProject{}
	for name, service := range config.Services {
		if service.RelativePath != "." || service.Language != project.ServiceLanguageDocker ||
			service.Host != project.ContainerAppTarget || service.Docker.Path != ".azd-spring/Dockerfile" {
			return nil, errors.New("spring run requires the root Maven service generated by azd spring init")
		}
		host := config.Resources[name]
		if host == nil || host.Type != project.ResourceTypeHostContainerApp {
			return nil, errors.New("the generated application host resource is missing")
		}
		props, ok := host.Props.(project.ContainerAppProps)
		if !ok || props.Port < 1 || props.Port > 65535 {
			return nil, errors.New("the generated application host configuration is invalid")
		}
		p.port = strconv.Itoa(props.Port)
		expected := map[string]string{
			"SPRING_PROFILES_ACTIVE": "", "SERVER_PORT": p.port,
			"SPRING_CONFIG_ADDITIONAL_LOCATION": "file:/app/config/application-azd.properties",
		}
		for _, env := range props.Env {
			value, known := expected[env.Name]
			if !known || env.Name != "SPRING_PROFILES_ACTIVE" && env.Value != value {
				return nil, errors.New("the application host environment differs from the generated launch configuration")
			}
			if env.Name == "SPRING_PROFILES_ACTIVE" {
				p.profiles = env.Value
			}
			delete(expected, env.Name)
		}
		if len(expected) != 0 {
			return nil, errors.New("the generated Spring launch environment is incomplete")
		}
		for _, profile := range splitList(p.profiles) {
			if !profileName.MatchString(profile) {
				return nil, errors.New("the generated Spring profile selection is invalid")
			}
		}
		if len(service.Uses) > 1 || !slices.Equal(service.Uses, host.Uses) {
			return nil, errors.New("multiple or inconsistent resource bindings are not supported by spring run")
		}
		for _, dependency := range service.Uses {
			resource := config.Resources[dependency]
			if resource == nil || resource.Type != databaseProviders[PostgreSQL].resource || resource.Existing {
				return nil, errors.New("spring run supports only embedded databases and generated PostgreSQL bindings")
			}
			p.database = resource.Name
			if p.database == "" {
				p.database = dependency
			}
		}
		if len(config.Resources) != 1+len(service.Uses) {
			return nil, errors.New("additional resource bindings are not supported by spring run")
		}
	}
	data, err = root.ReadFile("pom.xml")
	if err != nil {
		return nil, errors.New("pom.xml is required; run spring run in the generated Maven application directory")
	}
	var m model
	if xml.Unmarshal(data, &m) != nil || len(m.Modules) != 0 || (m.Packaging != "" && m.Packaging != "jar") {
		return nil, errors.New("spring run requires a single-module Maven JAR application")
	}
	data, err = root.ReadFile(".azd-spring/application-azd.properties")
	if err != nil {
		return nil, errors.New("the generated .azd-spring/application-azd.properties file is missing")
	}
	loader := properties.Loader{Encoding: properties.UTF8, DisableExpansion: true}
	binding, err := loader.LoadBytes(data)
	if err != nil {
		return nil, errors.New("the generated Spring properties file is invalid")
	}
	bindings := binding.Map()
	if p.database == "" {
		if len(bindings) != 0 {
			return nil, errors.New("the generated properties do not match the project's local-only binding")
		}
	} else {
		prefix := "jdbc:postgresql://${POSTGRES_HOST}:${POSTGRES_PORT}/${POSTGRES_DATABASE}?"
		options, found := strings.CutPrefix(bindings["spring.datasource.url"], prefix)
		validated, err := jdbcOptions(PostgreSQL, options)
		if !found || err != nil || validated != options || len(bindings) != 3 ||
			bindings["spring.datasource.username"] != "${POSTGRES_USERNAME}" ||
			bindings["spring.datasource.password"] != "${POSTGRES_PASSWORD}" {
			return nil, errors.New("the generated Spring properties do not match the PostgreSQL binding")
		}
	}
	return p, nil
}

func checkRunOverrides(ctx context.Context, directory string, p *runProject, parent []string) error {
	environment := mergeRunEnvironment(parent, nil, runtime.GOOS)
	lookup := func(key string) (string, bool) {
		for _, entry := range environment {
			name, value, _ := strings.Cut(entry, "=")
			if name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key) {
				return value, true
			}
		}
		return "", false
	}
	inherited := &configuration{values: map[string]property{}, lookup: lookup}
	if text, ok := lookup("SPRING_APPLICATION_JSON"); ok && text != "" {
		var values map[string]any
		if json.Unmarshal([]byte(text), &values) != nil {
			return errors.New("SPRING_APPLICATION_JSON must contain a valid JSON object")
		}
		if err := flatten(values, "", "SPRING_APPLICATION_JSON", inherited.values); err != nil {
			return err
		}
		inherited.lookup = func(key string) (string, bool) {
			if value, ok := inherited.values[canonical(key)]; ok {
				return value.text, true
			}
			return lookup(key)
		}
	}
	for key, expected := range map[string]string{
		"server.port":                       p.port,
		"spring.config.additional-location": "",
	} {
		if value, ok := inherited.raw(key); ok && value.text != expected {
			return fmt.Errorf("%s conflicts with the generated launch configuration; unset the inherited override", key)
		}
	}
	if p.profiles == "" {
		if value, ok := inherited.raw("spring.profiles.default"); ok && value.text != "none" {
			return errors.New("spring.profiles.default conflicts with the generated empty profile selection")
		}
	}
	if p.database != "" {
		for _, key := range []string{"spring.datasource.url", "spring.datasource.username", "spring.datasource.password"} {
			if _, ok := inherited.raw(key); ok {
				return fmt.Errorf("%s overrides the provisioned database binding; unset the inherited override", key)
			}
		}
	}
	for _, key := range []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "MAVEN_OPTS", "MAVEN_ARGS"} {
		value, _ := lookup(key)
		if strings.Contains(value, "spring.") || strings.Contains(value, "spring-boot.run.") {
			return fmt.Errorf("%s contains Spring launch overrides; remove them before running spring run", key)
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	c, err := readConfiguration(ctx, root, func(key string) (string, bool) {
		if p.profiles == "" && (key == "spring.profiles.default" || key == "SPRING_PROFILES_DEFAULT") {
			return "none", true
		}
		if _, explicit := inherited.raw("spring.profiles.active"); !explicit {
			if key == "SPRING_PROFILES_ACTIVE" || key == "spring.profiles.active" {
				return p.profiles, true
			}
		}
		return lookup(key)
	})
	if err != nil {
		return err
	}
	if !slices.Equal(c.profiles, splitList(p.profiles)) {
		return errors.New("spring.profiles activation differs from azure.yaml; restore the generated profile selection")
	}
	for _, key := range []string{
		"spring.datasource.jndi-name", "spring.datasource.hikari.jdbc-url",
		"spring.datasource.hikari.data-source-class-name",
		"spring.datasource.tomcat.url", "spring.datasource.dbcp2.url",
	} {
		if value, ok := c.raw(key); ok && value.text != "" {
			return fmt.Errorf("%s can override the generated database binding; remove this setting", key)
		}
	}
	if p.database != "" {
		for _, key := range []string{
			"spring.datasource.azure.passwordless-enabled", "spring.cloud.azure.jdbc.passwordless-enabled",
		} {
			value, err := c.resolve(key)
			if err != nil {
				return err
			}
			if value != "" && !strings.EqualFold(value, "false") {
				return fmt.Errorf("%s conflicts with the generated password-authenticated binding", key)
			}
		}
		driver, err := c.resolve("spring.datasource.driver-class-name")
		if err != nil {
			return err
		}
		if driver != "" && driver != "org.postgresql.Driver" {
			return errors.New("spring.datasource.driver-class-name conflicts with the generated PostgreSQL binding")
		}
		pool, err := c.resolve("spring.datasource.type")
		if err != nil {
			return err
		}
		if pool != "" && pool != "com.zaxxer.hikari.HikariDataSource" {
			return errors.New("spring.datasource.type selects a custom pool not supported by the PostgreSQL launcher")
		}
		for _, key := range []string{
			"spring.datasource.hikari.username", "spring.datasource.hikari.password",
			"spring.datasource.tomcat.username", "spring.datasource.tomcat.password",
			"spring.datasource.dbcp2.username", "spring.datasource.dbcp2.password",
		} {
			if _, ok := c.raw(key); ok {
				return fmt.Errorf("%s can override the provisioned database binding; remove this setting", key)
			}
		}
		for key := range c.values {
			if strings.HasPrefix(key, "spring.datasource.hikari.datasourceproperties.") {
				return errors.New(
					"spring.datasource.hikari.data-source-properties can override the provisioned database binding")
			}
		}
		for _, entry := range parent {
			key, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(strings.ToUpper(key), "SPRING_DATASOURCE_HIKARI_DATASOURCEPROPERTIES_") ||
				strings.HasPrefix(strings.ToUpper(key), "SPRING_DATASOURCE_HIKARI_DATA_SOURCE_PROPERTIES_") {
				return errors.New("inherited Hikari data-source-properties can override the provisioned database binding")
			}
		}
	} else {
		jdbc, err := c.resolve("spring.datasource.url")
		if err != nil {
			return err
		}
		if jdbc != "" && !strings.HasPrefix(jdbc, "jdbc:h2:mem:") &&
			!strings.HasPrefix(jdbc, "jdbc:derby:memory:") && !strings.HasPrefix(jdbc, "jdbc:hsqldb:mem:") {
			return errors.New(
				"spring.datasource.url conflicts with the generated local-only binding; restore the embedded configuration")
		}
	}
	return nil
}

func resolvePostgres(ctx context.Context, environment, database string) (map[string]string, error) {
	client, err := azdext.NewAzdClient()
	if err != nil {
		return nil, runError("connecting to azd", err, "Run this command through azd spring run.")
	}
	defer client.Close()
	return resolvePostgresEnvironment(ctx, environment, database, client.Environment(), client.Account(),
		func(ctx context.Context, tenant string, id *arm.ResourceID, vault string) (map[string]string, error) {
			credential, err := azdext.NewTokenProvider(ctx, client, &azdext.TokenProviderOptions{TenantID: tenant})
			if err != nil {
				return nil, runError("creating the user-access credential", err, "Run azd auth login.")
			}
			resources, err := armresources.NewClient(id.SubscriptionID, credential, nil)
			if err != nil {
				return nil, runError("creating the Azure resource client", err, "Check your azd Azure account.")
			}
			secrets, err := azdext.NewKeyVaultResolver(credential, nil)
			if err != nil {
				return nil, runError("creating the Key Vault client", err, "Check your azd Azure account.")
			}
			return readPostgresConnection(ctx, id, vault,
				func(ctx context.Context, server string) (any, error) {
					response, err := resources.GetByID(ctx, server, "2024-08-01", nil)
					return response.Properties, err
				}, secrets.Resolve)
		})
}

func resolvePostgresEnvironment(
	ctx context.Context, environment, database string,
	environments azdext.EnvironmentServiceClient, account azdext.AccountServiceClient,
	connect func(context.Context, string, *arm.ResourceID, string) (map[string]string, error),
) (map[string]string, error) {
	if environment == "" {
		current, err := environments.GetCurrent(ctx, &azdext.EmptyRequest{})
		if err != nil || current.GetEnvironment().GetName() == "" {
			return nil, runError("selecting the current azd environment", err, "Run azd provision first.")
		}
		environment = current.GetEnvironment().GetName()
	}
	response, err := environments.GetValues(ctx, &azdext.GetEnvironmentRequest{Name: environment})
	if err != nil {
		return nil, runError("reading the selected azd environment", err, "Select an existing provisioned azd environment.")
	}
	values := map[string]string{}
	for _, entry := range response.GetKeyValues() {
		values[entry.GetKey()] = entry.GetValue()
	}
	var databaseID *arm.ResourceID
	for key, value := range values {
		if !strings.HasPrefix(key, "AZURE_RESOURCE_") || !strings.HasSuffix(key, "_ID") {
			continue
		}
		id, err := arm.ParseResourceID(value)
		if err != nil ||
			!strings.EqualFold(id.ResourceType.String(), "Microsoft.DBforPostgreSQL/flexibleServers/databases") ||
			id.Name != database {
			continue
		}
		if databaseID != nil && !strings.EqualFold(databaseID.String(), id.String()) {
			return nil, errors.New("multiple provisioned PostgreSQL databases match the generated binding")
		}
		databaseID = id
	}
	if databaseID == nil {
		return nil, errors.New("the selected environment has no matching PostgreSQL output; run azd provision first")
	}
	if values["AZURE_SUBSCRIPTION_ID"] == "" ||
		!strings.EqualFold(values["AZURE_SUBSCRIPTION_ID"], databaseID.SubscriptionID) {
		return nil, errors.New("the PostgreSQL output does not match the selected environment's subscription")
	}
	vault, err := url.Parse(values["AZURE_KEY_VAULT_ENDPOINT"])
	if err != nil || vault.Scheme != "https" || vault.User != nil || vault.RawQuery != "" || vault.Fragment != "" ||
		vault.Port() != "" || !strings.HasSuffix(strings.ToLower(vault.Hostname()), ".vault.azure.net") ||
		(vault.Path != "" && vault.Path != "/") {
		return nil, errors.New(
			"the selected environment has an invalid AZURE_KEY_VAULT_ENDPOINT; check provisioning outputs")
	}
	tenant, err := account.LookupTenant(ctx, &azdext.LookupTenantRequest{SubscriptionId: databaseID.SubscriptionID})
	if err != nil || tenant.GetTenantId() == "" {
		return nil, runError("looking up the subscription's user-access tenant", err, "Run azd auth login.")
	}
	return connect(ctx, tenant.GetTenantId(), databaseID, strings.TrimSuffix(vault.String(), "/"))
}

func readPostgresConnection(
	ctx context.Context, id *arm.ResourceID, vault string,
	server func(context.Context, string) (any, error),
	secret func(context.Context, string) (string, error),
) (map[string]string, error) {
	raw, err := server(ctx, id.Parent.String())
	if err != nil {
		return nil, runError("reading the provisioned PostgreSQL server", err, "Check access to the provisioned server.")
	}
	props, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("the provisioned PostgreSQL server returned invalid metadata")
	}
	host, _ := props["fullyQualifiedDomainName"].(string)
	username, _ := props["administratorLogin"].(string)
	if host == "" || strings.ContainsAny(host, "/:@?# \t\r\n") || username == "" {
		return nil, errors.New(
			"the PostgreSQL server has no valid hostname or administrator login; password authentication is required")
	}
	if auth, ok := props["authConfig"].(map[string]any); ok && auth["passwordAuth"] == "Disabled" {
		return nil, errors.New(
			"the PostgreSQL server has password authentication disabled; managed identity is not supported")
	}
	password, err := secret(ctx, "@Microsoft.KeyVault(SecretUri="+vault+"/secrets/postgres-password)")
	if err != nil {
		return nil, runError("reading the PostgreSQL password from Key Vault", err,
			"Check that postgres-password exists and your signed-in account has secret-read access.")
	}
	if password == "" {
		return nil, errors.New("the provisioned postgres-password secret is empty; check the selected environment")
	}
	return map[string]string{
		"POSTGRES_HOST": host, "POSTGRES_PORT": "5432", "POSTGRES_DATABASE": id.Name,
		"POSTGRES_USERNAME": username, "POSTGRES_PASSWORD": password,
	}, nil
}

func mavenCommand(directory, goos string, runner exec.CommandRunner) (string, error) {
	wrapper := "mvnw"
	if goos == "windows" {
		wrapper = "mvnw.cmd"
	}
	file := filepath.Join(directory, wrapper)
	info, err := os.Stat(file)
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("the Maven wrapper is not a regular file")
		}
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking the Maven wrapper: %w", err)
	}
	if err := runner.ToolInPath("mvn"); err != nil {
		return "", &azdext.ToolError{
			Message: "Maven is not available", ToolName: "maven", Kind: azdext.ToolErrorKindMissing,
			Suggestion: "Add the platform's Maven wrapper to this project or install Maven on PATH.",
		}
	}
	return "mvn", nil
}

func fileURI(absolutePath, goos string) string {
	path := absolutePath
	if goos == "windows" {
		path = strings.ReplaceAll(path, `\`, "/")
		if strings.HasPrefix(path, "//") {
			host, remainder, _ := strings.Cut(path[2:], "/")
			return (&url.URL{Scheme: "file", Host: host, Path: "/" + remainder}).String()
		}
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func mergeRunEnvironment(parent []string, values map[string]string, goos string) []string {
	merged := map[string]string{}
	keyFor := func(key string) string {
		if goos == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if goos == "windows" && strings.HasPrefix(entry, "=") {
			drive, _, _ := strings.Cut(entry[1:], "=")
			key = "=" + drive
		}
		merged[keyFor(key)] = entry
	}
	for key, value := range values {
		merged[keyFor(key)] = key + "=" + value
	}
	keys := slices.Sorted(maps.Keys(merged))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, merged[key])
	}
	return result
}

func runError(action string, err error, suggestion string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := action + " failed"
	if response, ok := errors.AsType[*azcore.ResponseError](err); ok {
		message += fmt.Sprintf(" (HTTP %d)", response.StatusCode)
	}
	// SDK errors can embed credential-bearing URLs or service response bodies.
	return &azdext.LocalError{
		Message: message, Code: "local_run_failed", Category: azdext.LocalErrorCategoryDependency,
		Suggestion: suggestion,
	}
}
