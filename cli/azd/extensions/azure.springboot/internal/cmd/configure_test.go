// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testEnvironment struct {
	azdext.UnimplementedEnvironmentServiceServer
	mu     sync.Mutex
	values map[string]string
	name   string
	fail   string
	writes []string
}

func (s *testEnvironment) GetValues(_ context.Context, req *azdext.GetEnvironmentRequest) (
	*azdext.KeyValueListResponse, error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = req.Name
	response := &azdext.KeyValueListResponse{}
	for key, value := range s.values {
		response.KeyValues = append(response.KeyValues, &azdext.KeyValue{Key: key, Value: value})
	}
	return response, nil
}

func (s *testEnvironment) SetValue(_ context.Context, req *azdext.SetEnvRequest) (*azdext.EmptyResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.EnvName != s.name {
		return nil, status.Error(codes.InvalidArgument, "wrong selected environment")
	}
	if req.Key == s.fail {
		return nil, status.Error(codes.Internal, "test persistence failure")
	}
	s.values[req.Key] = req.Value
	s.writes = append(s.writes, req.Key)
	return &azdext.EmptyResponse{}, nil
}

func configureFixture(t *testing.T, state *testEnvironment) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	azdext.RegisterEnvironmentServiceServer(server, state)
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-finished; err != nil {
			t.Errorf("gRPC fixture shutdown: %v", err)
		}
	})
	t.Setenv("AZD_SERVER", listener.Addr().String())
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "api"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"azure.yaml", filepath.Join("api", "pom.xml"), ".springboot-template"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("test fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificatePath := filepath.Join(dir, "test-public-root.pem")
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certificatePath, certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.values["POSTGRES_DEVELOPER_SSL_ROOT_CERT"] = certificatePath
	state.mu.Unlock()
	t.Setenv("AZD_EXEC_PROJECT_DIR", dir)
	return dir
}

func provisionedValues() map[string]string {
	return map[string]string{
		"POSTGRES_HOST": "sample.postgres.database.azure.com", "POSTGRES_DATABASE": "notes",
		"POSTGRES_USERNAME": "developer@example.com", "POSTGRES_MANAGED_IDENTITY_USERNAME": "notes-app",
	}
}

func TestConfigureSelectedEnvironmentAndRepeat(t *testing.T) {
	state := &testEnvironment{values: provisionedValues()}
	dir := configureFixture(t, state)
	var output bytes.Buffer
	for range 2 {
		command := NewRootCommand(&output, io.Discard)
		command.SetArgs([]string{"configure", "--project-dir", dir, "-e", "selected"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.name != "selected" || state.values["SPRING_DATASOURCE_USERNAME"] != "developer@example.com" ||
		state.values["SPRINGBOOT_DEPLOYED_SPRING_DATASOURCE_USERNAME"] != "notes-app" {
		t.Fatal("configuration was not delivered to the selected environment with separate principals")
	}
	if !strings.Contains(output.String(), "validated and saved") {
		t.Fatal("configuration contribution missing from injected writer")
	}
}

func TestConfigureValidatesAllSettingsBeforeWriting(t *testing.T) {
	values := provisionedValues()
	values["SPRINGBOOT_DEPLOYED_SPRING_DATASOURCE_USERNAME"] = "explicit-other"
	state := &testEnvironment{values: values}
	dir := configureFixture(t, state)
	command := NewRootCommand(io.Discard, io.Discard)
	command.SetArgs([]string{"configure", "--project-dir", dir, "-e", "selected"})
	if err := command.Execute(); err == nil {
		t.Fatal("explicit setting conflict was ignored")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.writes) != 0 {
		t.Fatal("developer settings were partially saved before validating deployed settings")
	}
}

func TestConfigureReportsPersistenceFailureWithoutAdvancingTracking(t *testing.T) {
	values := provisionedValues()
	values["SPRING_DATASOURCE_URL"] = "old"
	values["SPRINGBOOT_PREVIOUS_SPRING_DATASOURCE_URL"] = "old"
	state := &testEnvironment{values: maps.Clone(values), fail: "SPRING_DATASOURCE_URL"}
	dir := configureFixture(t, state)
	command := NewRootCommand(io.Discard, io.Discard)
	command.SetArgs([]string{"configure", "--project-dir", dir, "-e", "selected"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "saving SPRING_DATASOURCE_URL") {
		t.Fatalf("error=%v", err)
	}
	state.mu.Lock()
	if state.values["SPRINGBOOT_PREVIOUS_SPRING_DATASOURCE_URL"] != "old" {
		t.Fatal("failed write advanced the tracking value")
	}
	state.fail = ""
	state.mu.Unlock()
	command = NewRootCommand(io.Discard, io.Discard)
	command.SetArgs([]string{"configure", "--project-dir", dir, "-e", "selected"})
	if err := command.Execute(); err != nil {
		t.Fatalf("recovering partial persistence: %v", err)
	}
}

func TestConfigureRejectsDifferentHostProject(t *testing.T) {
	state := &testEnvironment{values: provisionedValues()}
	dir := configureFixture(t, state)
	t.Setenv("AZD_EXEC_PROJECT_DIR", t.TempDir())
	command := NewRootCommand(io.Discard, io.Discard)
	command.SetArgs([]string{"configure", "--project-dir", dir, "-e", "selected"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project-dir") || !strings.Contains(err.Error(), "--cwd") {
		t.Fatalf("error=%v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.writes) != 0 {
		t.Fatal("settings were written into a different host project's environment")
	}
}
