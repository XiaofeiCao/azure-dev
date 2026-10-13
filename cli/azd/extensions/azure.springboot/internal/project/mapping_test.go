// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"maps"
	"strings"
	"testing"
)

func TestSpringSettings(t *testing.T) {
	base := map[string]string{
		"POSTGRES_HOST":     "example.postgres.database.azure.com",
		"POSTGRES_DATABASE": "notes", "POSTGRES_USERNAME": "developer@example.com",
	}

	tests := []struct {
		name string
		edit map[string]string
		fail bool
	}{
		{"valid", nil, false},
		{"bad host", map[string]string{"POSTGRES_HOST": "https://user:secret@example.com/?sig=secret"}, true},
		{"missing host", map[string]string{"POSTGRES_HOST": ""}, true},
		{"bad database", map[string]string{"POSTGRES_DATABASE": "../notes"}, true},
		{"bad port", map[string]string{"POSTGRES_PORT": "65536"}, true},
		{"missing username", map[string]string{"POSTGRES_USERNAME": ""}, true},
		{"username injection", map[string]string{"POSTGRES_USERNAME": "developer\npassword=secret"}, true},
		{"explicit conflict", map[string]string{"SPRING_DATASOURCE_URL": "jdbc:postgresql://other/db"}, true},
		{"tracked refresh", map[string]string{
			"SPRING_DATASOURCE_URL": "old", "SPRINGBOOT_PREVIOUS_SPRING_DATASOURCE_URL": "old",
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := maps.Clone(base)
			maps.Copy(values, test.edit)
			settings, err := SpringSettings(values)
			if (err != nil) != test.fail {
				t.Fatalf("settings=%v error=%v", settings, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("validation error disclosed credentials")
			}
			if !test.fail {
				if !strings.Contains(settings["SPRING_DATASOURCE_URL"], "sslmode=verify-full") {
					t.Fatal("TLS server verification is missing")
				}
				maps.Copy(values, settings)
				if _, err := SpringSettings(values); err != nil {
					t.Fatalf("repeat configuration: %v", err)
				}
			}
		})
	}
}

func TestDeveloperAndManagedIdentitySettingsAreSeparate(t *testing.T) {
	values := map[string]string{
		"POSTGRES_HOST": "example.postgres.database.azure.com", "POSTGRES_DATABASE": "notes",
		"POSTGRES_USERNAME": "developer@example.com", "POSTGRES_MANAGED_IDENTITY_USERNAME": "notes-app",
		"SPRING_DATASOURCE_USERNAME": "developer@example.com",
	}
	deployed, err := DeployedSpringSettings(values)
	if err != nil {
		t.Fatal(err)
	}
	if deployed["SPRINGBOOT_DEPLOYED_SPRING_DATASOURCE_USERNAME"] != "notes-app" {
		t.Fatal("managed identity was replaced by developer identity")
	}
	if values["SPRING_DATASOURCE_USERNAME"] != "developer@example.com" {
		t.Fatal("developer settings were mutated")
	}
	maps.Copy(values, deployed)
	if _, err := DeployedSpringSettings(values); err != nil {
		t.Fatalf("repeat configuration: %v", err)
	}
	values["SPRINGBOOT_DEPLOYED_SPRING_DATASOURCE_USERNAME"] = "explicit-other"
	if _, err := DeployedSpringSettings(values); err == nil {
		t.Fatal("explicit deployed setting conflict was ignored")
	}
}
