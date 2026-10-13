// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,62}$`)
var hostname = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$`)

// SpringSettings validates provisioning outputs and preserves explicit Spring settings.
func SpringSettings(values map[string]string) (map[string]string, error) {
	host := values["POSTGRES_HOST"]
	if len(host) > 253 || (!hostname.MatchString(host) && net.ParseIP(host) == nil) ||
		strings.Contains(host, "..") {
		return nil, fmt.Errorf("POSTGRES_HOST must be a hostname or IP address, not a URL")
	}

	database := values["POSTGRES_DATABASE"]
	if !identifier.MatchString(database) {
		return nil, fmt.Errorf("POSTGRES_DATABASE must be a simple PostgreSQL identifier")
	}
	port := values["POSTGRES_PORT"]
	if port == "" {
		port = "5432"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return nil, fmt.Errorf("POSTGRES_PORT must be between 1 and 65535")
	}
	username := values["POSTGRES_USERNAME"]
	if strings.TrimSpace(username) == "" || strings.ContainsAny(username, "\r\n\x00") {
		return nil, fmt.Errorf("POSTGRES_USERNAME must name the developer's Entra PostgreSQL principal")
	}
	const plugin = "com.azure.identity.extensions.jdbc.postgresql.AzurePostgresqlAuthenticationPlugin"
	if schema := values["POSTGRES_SCHEMA"]; schema != "" && schema != "notes" {
		return nil, fmt.Errorf("POSTGRES_SCHEMA must be notes for this prepared template's bootstrap")
	}
	query := url.Values{
		"sslmode": {"verify-full"}, "authenticationPluginClassName": {plugin}, "currentSchema": {"notes"},
	}
	jdbc := "jdbc:postgresql://" + net.JoinHostPort(host, port) + "/" + database + "?" + query.Encode()
	generated := map[string]string{
		"SPRING_DATASOURCE_URL":      jdbc,
		"SPRING_DATASOURCE_USERNAME": username,
		"SPRING_PROFILES_ACTIVE":     "azure",
	}
	result := map[string]string{}
	for key, desired := range generated {
		current := values[key]
		previous := values["SPRINGBOOT_PREVIOUS_"+key]
		if current != "" && current != desired && current != previous {
			return nil, fmt.Errorf(
				"%s conflicts with PostgreSQL provisioning outputs; remove the explicit override or align it", key)
		}
		result[key] = desired
		result["SPRINGBOOT_PREVIOUS_"+key] = desired
	}
	return result, nil
}

// DeployedSpringSettings keeps managed-identity settings separate from developer settings.
func DeployedSpringSettings(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("missing provisioned PostgreSQL outputs")
	}
	deployed := maps.Clone(values)
	deployed["POSTGRES_USERNAME"] = values["POSTGRES_MANAGED_IDENTITY_USERNAME"]
	for _, key := range []string{"SPRING_DATASOURCE_URL", "SPRING_DATASOURCE_USERNAME", "SPRING_PROFILES_ACTIVE"} {
		deployed[key] = values["SPRINGBOOT_DEPLOYED_"+key]
		deployed["SPRINGBOOT_PREVIOUS_"+key] = values["SPRINGBOOT_PREVIOUS_SPRINGBOOT_DEPLOYED_"+key]
	}
	settings, err := SpringSettings(deployed)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for key, value := range settings {
		if suffix, tracking := strings.CutPrefix(key, "SPRINGBOOT_PREVIOUS_"); tracking {
			result["SPRINGBOOT_PREVIOUS_SPRINGBOOT_DEPLOYED_"+suffix] = value
		} else {
			result["SPRINGBOOT_DEPLOYED_"+key] = value
		}
	}
	return result, nil
}
