// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// notInCluster makes rest.InClusterConfig fail deterministically, so these
// tests exercise the tolerant no-client path even if they ever run inside a pod.
func notInCluster(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func TestHelpExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	require.Equal(t, 0, run([]string{"help"}, &out, &errOut))
	require.Contains(t, out.String(), "nvml-mock-node")
}

func TestNoCommandIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	require.Equal(t, 2, run(nil, &out, &errOut))
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	require.Equal(t, 2, run([]string{"bogus"}, &out, &errOut))
	require.Contains(t, errOut.String(), `unknown command "bogus"`)
}

func TestInvalidPCIVendorLabelIsFatal(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--pci-vendor-label", "maybe", "label"}, &out, &errOut)
	require.Equal(t, 2, code, "a typo must fail the pod, not silently disable the gate")
	require.Contains(t, errOut.String(), "expected on or off")
}

func TestLabelTakesTheNodeNameFromTheEnvironment(t *testing.T) {
	notInCluster(t)
	t.Setenv("NODE_NAME", "env-node")
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	code := run([]string{"--features-dir", dir, "label"}, &out, &errOut)

	require.Equal(t, 0, code, "a missing client must not crash-loop the mock")
	require.Contains(t, errOut.String(), "env-node")
	require.FileExists(t, filepath.Join(dir, "nvml-mock.features"),
		"the feature file does not depend on the API server")
}

func TestLabelPrefersTheFlagOverTheEnvironment(t *testing.T) {
	notInCluster(t)
	t.Setenv("NODE_NAME", "env-node")

	var out, errOut bytes.Buffer
	code := run([]string{"--node-name", "flag-node", "--features-dir", t.TempDir(), "label"}, &out, &errOut)

	require.Equal(t, 0, code)
	require.Contains(t, errOut.String(), "flag-node")
	require.NotContains(t, errOut.String(), "env-node")
}

func TestLabelWithoutANodeNameStillWritesTheFeatureFile(t *testing.T) {
	notInCluster(t)
	t.Setenv("NODE_NAME", "")
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	code := run([]string{"--features-dir", dir, "label"}, &out, &errOut)

	// The NFD input needs no node identity at all; only the direct label does.
	require.Equal(t, 0, code)
	require.FileExists(t, filepath.Join(dir, "nvml-mock.features"))
}

func TestLabelGateOffRemovesAStaleFeatureFile(t *testing.T) {
	notInCluster(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "nvml-mock.features")
	require.NoError(t, os.WriteFile(path, []byte("pci-10de.present=true\n"), 0o644))

	var out, errOut bytes.Buffer
	code := run([]string{"--node-name", "n", "--features-dir", dir, "--pci-vendor-label", "off", "label"}, &out, &errOut)

	require.Equal(t, 0, code)
	require.NoFileExists(t, path)
}

func TestLabelAcceptsFlagsAfterTheCommand(t *testing.T) {
	notInCluster(t)
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	// The form setup.sh uses and the usage text documents.
	code := run([]string{"label", "--node-name", "n1", "--features-dir", dir}, &out, &errOut)

	require.Equal(t, 0, code)
	require.FileExists(t, filepath.Join(dir, "nvml-mock.features"))
	require.Contains(t, errOut.String(), "n1")
}

func TestTeardownAcceptsFlagsAfterTheCommand(t *testing.T) {
	notInCluster(t)
	root := t.TempDir()
	cdi := filepath.Join(root, "var/run/cdi")
	require.NoError(t, os.MkdirAll(cdi, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cdi, "nvidia.yaml"), []byte("x"), 0o644))

	var out, errOut bytes.Buffer
	code := run([]string{"teardown", "--node-name", "n1", "--host-root", root}, &out, &errOut)

	require.Equal(t, 0, code)
	require.NoFileExists(t, filepath.Join(cdi, "nvidia.yaml"))
}

func TestASecondBareArgumentIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	require.Equal(t, 2, run([]string{"label", "bogus"}, &out, &errOut))
	require.Contains(t, errOut.String(), `unexpected argument "bogus"`)
}

func TestTeardownCleansTheHostRoot(t *testing.T) {
	notInCluster(t)
	root := t.TempDir()
	mock := filepath.Join(root, "var/lib/nvml-mock")
	require.NoError(t, os.MkdirAll(filepath.Join(mock, "driver"), 0o755))
	cdi := filepath.Join(root, "var/run/cdi")
	require.NoError(t, os.MkdirAll(cdi, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cdi, "nvidia.yaml"), []byte("x"), 0o644))

	var out, errOut bytes.Buffer
	code := run([]string{"--node-name", "n", "--host-root", root, "teardown"}, &out, &errOut)

	require.Equal(t, 0, code, "no cluster is a warning, not a teardown failure")
	require.NoFileExists(t, filepath.Join(cdi, "nvidia.yaml"))
	require.DirExists(t, mock)
	entries, err := os.ReadDir(mock)
	require.NoError(t, err)
	require.Empty(t, entries)
}
