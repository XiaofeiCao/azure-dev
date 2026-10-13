// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateCertificate(path string) error {
	if path == "" {
		return fmt.Errorf("POSTGRES_DEVELOPER_SSL_ROOT_CERT must identify a readable PEM certificate bundle")
	}
	if strings.Contains(path, "://") {
		return fmt.Errorf("POSTGRES_DEVELOPER_SSL_ROOT_CERT must be a local file path, not a URL")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the configured PEM certificate bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return fmt.Errorf("configured PostgreSQL trust bundle contains no valid PEM certificates")
	}
	return nil
}

func developerCertificate(ctx context.Context, directory, configured string, out, errOut io.Writer) (string, error) {
	if configured != "" {
		return configured, validateCertificate(configured)
	}
	local := filepath.Join(directory, ".local")
	path := filepath.Join(local, "postgresql-roots.pem")
	for _, candidate := range []string{local, path} {
		info, err := os.Lstat(candidate)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("the local PostgreSQL trust path must not be a symbolic link")
		}
	}
	if _, err := os.Stat(path); err == nil {
		return path, validateCertificate(path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(local, 0700); err != nil {
		return "", err
	}
	var created strings.Builder
	if err := execute(ctx, "docker", []string{"create", "--entrypoint", "/bin/true", runImage},
		directory, os.Environ(), &created, errOut); err != nil {
		return "", err
	}
	id := strings.TrimSpace(created.String())
	if !containerID.MatchString(id) {
		return "", fmt.Errorf("Docker returned an invalid certificate-container identifier")
	}
	copyErr := execute(ctx, "docker", []string{"cp", id + ":/etc/ssl/certs/ca-certificates.crt", path},
		directory, os.Environ(), out, errOut)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cleanupErr := execute(cleanupCtx, "docker", []string{"rm", id}, directory, os.Environ(), io.Discard, errOut)
	if copyErr != nil || cleanupErr != nil {
		return "", fmt.Errorf("exporting public PostgreSQL trust (copy: %v; cleanup: %v)", copyErr, cleanupErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if block, _ := pem.Decode(data); block == nil {
		return "", fmt.Errorf("exported trust bundle is not PEM")
	}
	return path, validateCertificate(path)
}
