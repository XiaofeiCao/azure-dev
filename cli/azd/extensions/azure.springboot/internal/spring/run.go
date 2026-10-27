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
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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
	binding  runBinding
}

type runBinding struct {
	ServiceName         string   `json:"serviceName"`
	Port                int      `json:"port"`
	Profiles            []string `json:"profiles"`
	Engine              string   `json:"engine"`
	Authentication      string   `json:"authentication"`
	DatabaseName        string   `json:"databaseName"`
	EnvironmentBindings bool     `json:"environmentBindings"`
	JDBCOptions         string   `json:"jdbcOptions"`
	Driver              string   `json:"driver"`
	EmbeddedURL         string   `json:"embeddedUrl"`
	EmbeddedConfigured  bool     `json:"embeddedUrlConfigured"`
	EmbeddedDriver      string   `json:"embeddedDriver"`
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
	if options.EnvironmentExplicit && strings.TrimSpace(options.Environment) == "" {
		return errors.New("--environment requires a non-empty azd environment name; " +
			"omit --environment to use the current environment")
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
		"SPRING_PROFILES_ACTIVE":  p.profiles,
		"SPRING_PROFILES_DEFAULT": "none",
		"SERVER_PORT":             p.port,
	}
	if err := checkRunOverrides(ctx, directory, p, parent); err != nil {
		return err
	}
	command, err := mavenCommand(directory, runtime.GOOS, runner)
	if err != nil {
		return err
	}
	if p.database != "" {
		if err := checkRunTLS(p.binding.JDBCOptions, runtime.GOOS); err != nil {
			return err
		}
		resolved, err := resolve(ctx, options.Environment, p.database)
		if err != nil {
			return err
		}
		maps.Copy(values, resolved)
	}
	for key, value := range p.datasourceProperties(values["POSTGRES_HOST"], values["POSTGRES_PASSWORD"]) {
		name := strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
		values[name] = value
	}
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
	if len(config.Resources) != 0 {
		return nil, errors.New(
			"legacy Spring resource projects are not supported; generate a fresh project with updated init")
	}
	if config.Infra.Provider != "bicep" || config.Infra.Path != "infra" || config.Infra.Module != "main" {
		return nil, errors.New("spring run requires the generated infra/main Bicep project")
	}
	main, err := readRunParameters(root, "infra/main.parameters.json")
	if err != nil {
		return nil, err
	}
	application, err := readRunParameters(root, "infra/application.parameters.json")
	if err != nil {
		return nil, err
	}
	for destination, source := range map[string]string{
		"engine": "databaseEngine", "authentication": "databaseAuth", "password": "databasePassword",
	} {
		main[destination] = main[source]
	}
	binding, err := decodeRunBinding(application)
	if err != nil {
		return nil, err
	}
	mainBinding, err := decodeRunBinding(main)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(binding, mainBinding) {
		return nil, errors.New("main and application parameters disagree; restore the generated launch configuration")
	}
	if !runParameterEquals(main, "password", "${SPRING_DATABASE_PASSWORD=}") ||
		!runParameterEquals(application, "password", "${SPRING_DATABASE_PASSWORD=}") ||
		!runParameterEquals(application, "host", "${SPRING_DATABASE_HOST=}") ||
		!runParameterEquals(application, "username", "azdadmin") {
		return nil, errors.New("the generated host, username, or password parameter binding is invalid")
	}
	p := &runProject{binding: *binding, port: strconv.Itoa(binding.Port), profiles: strings.Join(binding.Profiles, ",")}
	if binding.Engine == string(PostgreSQL) {
		p.database = binding.DatabaseName
	}
	for name, service := range config.Services {
		dockerfile := ".azd-spring/Dockerfile"
		if binding.EnvironmentBindings {
			dockerfile = "Dockerfile"
		}
		if service.RelativePath != "." || service.Language != project.ServiceLanguageDocker ||
			service.Host != project.ContainerAppTarget || service.Docker.Path != dockerfile ||
			service.Docker.Context != "." || service.Module != "application" ||
			name != binding.ServiceName || len(service.Uses) != 0 {
			return nil, errors.New("spring run requires the root Maven service generated by azd spring init")
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
	if binding.EnvironmentBindings {
		return p, nil
	}
	data, err = root.ReadFile(".azd-spring/application-azd.properties")
	if err != nil {
		return nil, errors.New("the generated .azd-spring/application-azd.properties file is missing")
	}
	loader := properties.Loader{Encoding: properties.UTF8, DisableExpansion: true}
	propertiesFile, err := loader.LoadBytes(data)
	if err != nil {
		return nil, errors.New("the generated Spring properties file is invalid")
	}
	bindings := propertiesFile.Map()
	expected := p.datasourceProperties("${POSTGRES_HOST}", "${POSTGRES_PASSWORD}")
	if p.database != "" {
		expected["spring.datasource.url"] = "jdbc:postgresql://${POSTGRES_HOST}:${POSTGRES_PORT}/${POSTGRES_DATABASE}?" +
			binding.JDBCOptions
		expected["spring.datasource.username"] = "${POSTGRES_USERNAME}"
	}
	if !maps.Equal(bindings, expected) {
		return nil, errors.New("the generated Spring properties do not match the saved launch binding")
	}
	return p, nil
}

func readRunParameters(root *os.Root, file string) (map[string]json.RawMessage, error) {
	data, err := root.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s is required; generate a fresh project with updated azd spring init", file)
	}
	var document struct {
		Parameters map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &document) != nil || document.Parameters == nil {
		return nil, fmt.Errorf("%s must contain generated nested parameter values", file)
	}
	values := make(map[string]json.RawMessage, len(document.Parameters))
	for key, parameter := range document.Parameters {
		values[key] = parameter.Value
	}
	return values, nil
}

func decodeRunBinding(values map[string]json.RawMessage) (*runBinding, error) {
	for _, key := range []string{
		"serviceName", "port", "profiles", "engine", "authentication", "databaseName", "environmentBindings",
		"jdbcOptions", "driver", "embeddedUrl", "embeddedUrlConfigured", "embeddedDriver",
	} {
		if len(values[key]) == 0 || string(values[key]) == "null" {
			return nil, fmt.Errorf("generated launch parameter %s is missing or null", key)
		}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, errors.New("the generated launch parameters are invalid")
	}
	var binding runBinding
	if json.Unmarshal(data, &binding) != nil || !containerAppName.MatchString(binding.ServiceName) ||
		binding.Port < 1 || binding.Port > 65535 {
		return nil, errors.New("the generated launch parameter types, service name, or port are invalid")
	}
	seen := map[string]bool{}
	for _, profile := range binding.Profiles {
		if len(binding.Profiles) > 64 || !profileName.MatchString(profile) || seen[profile] {
			return nil, errors.New("the generated Spring profile selection is invalid")
		}
		seen[profile] = true
	}
	if binding.Authentication != "password" || binding.Engine != "none" && binding.Engine != string(PostgreSQL) {
		return nil, errors.New("spring run supports only embedded databases and PostgreSQL password authentication")
	}
	if binding.Engine == "none" {
		if binding.DatabaseName != "" || binding.Driver != "" || binding.JDBCOptions != "" {
			return nil, errors.New("the generated local-only binding contains external database settings")
		}
		if binding.EmbeddedURL != "" && !binding.EmbeddedConfigured {
			return nil, errors.New("the generated embedded URL configuration is inconsistent")
		}
		for _, engine := range embeddedEngines {
			if binding.EmbeddedURL != "" && strings.HasPrefix(binding.EmbeddedURL, engine.prefix) ||
				binding.EmbeddedURL == "" && binding.EmbeddedDriver == engine.driver {
				err := validateEmbedded(&EmbeddedBinding{
					Engine: engine.name, URL: binding.EmbeddedURL, Driver: binding.EmbeddedDriver,
				})
				if err != nil {
					return nil, errors.New("the generated embedded datasource binding is invalid")
				}
				return &binding, nil
			}
		}
		if binding.EmbeddedURL != "" || binding.EmbeddedDriver != "" {
			return nil, errors.New("the generated embedded datasource binding is unsupported")
		}
	} else if !databaseName.MatchString(binding.DatabaseName) ||
		validateExternalDriver(PostgreSQL, binding.Driver) != nil ||
		validateRunJDBCOptions(binding.JDBCOptions) != nil ||
		binding.EmbeddedURL != "" || binding.EmbeddedConfigured || binding.EmbeddedDriver != "" {
		return nil, errors.New("the generated PostgreSQL binding is invalid")
	}
	return &binding, nil
}

func runParameterEquals(values map[string]json.RawMessage, key, expected string) bool {
	var value string
	return json.Unmarshal(values[key], &value) == nil && value == expected
}

func validateRunJDBCOptions(options string) error {
	values, err := url.ParseQuery(options)
	if err != nil {
		return err
	}
	values.Del("sslrootcert")
	expected, err := jdbcOptions(PostgreSQL, values.Encode())
	if err != nil || expected != options {
		return errors.New("PostgreSQL JDBC options differ from generated settings")
	}
	return nil
}

func checkRunTLS(options, goos string) error {
	values, err := url.ParseQuery(options)
	if err != nil {
		return errors.New("the generated PostgreSQL JDBC options are invalid")
	}
	if certificate := values.Get("sslrootcert"); certificate != "" {
		if goos != "windows" {
			if info, err := os.Stat(certificate); err == nil && info.Mode().IsRegular() {
				return nil
			}
		}
		return errors.New(
			"PostgreSQL certificate verification (sslmode=verify-ca/verify-full) for local execution is supported only on Linux; " +
				"Windows and macOS are not supported. Run on Linux with /etc/ssl/certs/ca-certificates.crt available; " +
				"TLS verification is never downgraded",
		)
	}
	return nil
}

func (p *runProject) datasourceProperties(host, password string) map[string]string {
	values := map[string]string{}
	if p.database != "" {
		values["spring.datasource.url"] = "jdbc:postgresql://" + host + ":5432/" + p.database + "?" + p.binding.JDBCOptions
		values["spring.datasource.username"] = "azdadmin"
		values["spring.datasource.password"] = password
		if p.binding.Driver != "" {
			values["spring.datasource.driver-class-name"] = p.binding.Driver
		}
	} else {
		if p.binding.EmbeddedConfigured || p.binding.EmbeddedURL != "" {
			values["spring.datasource.url"] = p.binding.EmbeddedURL
		}
		if p.binding.EmbeddedDriver != "" {
			values["spring.datasource.driver-class-name"] = p.binding.EmbeddedDriver
		}
	}
	return values
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
	if value, ok := inherited.raw("spring.profiles.default"); ok && value.text != "none" {
		return errors.New("spring.profiles.default conflicts with the generated profile selection")
	}
	if p.database != "" {
		for _, key := range []string{"spring.datasource.url", "spring.datasource.username", "spring.datasource.password"} {
			if _, ok := inherited.raw(key); ok {
				return fmt.Errorf("%s overrides the provisioned database binding; unset the inherited override", key)
			}
		}
	} else {
		for key, expected := range p.datasourceProperties("", "") {
			if value, ok := inherited.raw(key); ok && value.text != expected {
				return fmt.Errorf("%s conflicts with the saved embedded binding; unset the inherited override", key)
			}
		}
	}
	for _, key := range []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "MAVEN_OPTS", "MAVEN_ARGS"} {
		value, _ := lookup(key)
		if strings.Contains(value, "spring.") || strings.Contains(value, "spring-boot.run.") ||
			strings.Contains(value, "server.port") {
			return fmt.Errorf("%s contains Spring launch overrides; remove them before running spring run", key)
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	c, err := readConfiguration(ctx, root, func(key string) (string, bool) {
		if key == "spring.profiles.default" || key == "SPRING_PROFILES_DEFAULT" {
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
		return errors.New(
			"spring.profiles activation differs from saved parameters; restore the generated profile selection")
	}
	for _, key := range []string{
		"spring.datasource.hikari.jdbc-url",
		"spring.datasource.hikari.data-source-class-name", "spring.datasource.hikari.driver-class-name",
		"spring.datasource.hikari.username", "spring.datasource.hikari.password",
		"spring.datasource.tomcat.url", "spring.datasource.tomcat.username", "spring.datasource.tomcat.password",
		"spring.datasource.tomcat.driver-class-name", "spring.datasource.dbcp2.url",
		"spring.datasource.dbcp2.username", "spring.datasource.dbcp2.password", "spring.datasource.dbcp2.driver-class-name",
	} {
		if _, ok := c.raw(key); ok {
			return fmt.Errorf("%s can override the generated database binding; remove this setting", key)
		}
	}
	jndi, err := c.resolve("spring.datasource.jndi-name")
	if err != nil {
		return err
	}
	if jndi != "" {
		return errors.New("spring.datasource.jndi-name can override the generated database binding; remove this setting")
	}
	for key := range c.values {
		if strings.HasPrefix(key, "spring.datasource.hikari.datasourceproperties.") {
			return errors.New("spring.datasource.hikari.data-source-properties can override the generated database binding")
		}
	}
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "SPRING_DATASOURCE_HIKARI_DATASOURCEPROPERTIES_") ||
			strings.HasPrefix(strings.ToUpper(key), "SPRING_DATASOURCE_HIKARI_DATA_SOURCE_PROPERTIES_") {
			return errors.New("inherited Hikari data-source-properties can override the generated database binding")
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
	} else if !p.binding.EmbeddedConfigured && p.binding.EmbeddedURL == "" {
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
	return resolvePostgresEnvironment(ctx, environment, database, client.Environment())
}

func resolvePostgresEnvironment(
	ctx context.Context, environment, database string,
	environments azdext.EnvironmentServiceClient,
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
	host, server := values["SPRING_DATABASE_HOST"], values["SPRING_DATABASE_SERVER_NAME"]
	if values["SPRING_DATABASE_NAME"] != database || !databaseName.MatchString(database) {
		return nil, errors.New(
			"the selected environment has no matching PostgreSQL database output; run azd provision first")
	}
	if server == "" || len(server) > 63 || strings.Trim(server, "-") != server ||
		strings.Trim(server, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
		host != server+".postgres.database.azure.com" {
		return nil, errors.New(
			"the selected environment has invalid PostgreSQL host/server outputs; run azd provision first")
	}
	password := values["SPRING_DATABASE_PASSWORD"]
	if password == "" {
		return nil, errors.New(
			"SPRING_DATABASE_PASSWORD is missing or empty in the selected environment; run azd provision first")
	}
	return map[string]string{
		"POSTGRES_HOST": host, "POSTGRES_PORT": "5432", "POSTGRES_DATABASE": database,
		"POSTGRES_USERNAME": "azdadmin", "POSTGRES_PASSWORD": password,
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
