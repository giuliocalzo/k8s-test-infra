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

package labels

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubPatcher records what would have been sent to the API server. A
// hand-written stub rather than client-go's generated fake clientset: the
// interface is one method wide, and the fake would add hundreds of files to
// vendor/, which the image build consumes with -mod=vendor.
type stubPatcher struct {
	nodes   []string
	patches [][]byte
	err     error
}

func (s *stubPatcher) Patch(_ context.Context, nodeName string, patch []byte) error {
	s.nodes = append(s.nodes, nodeName)
	s.patches = append(s.patches, patch)
	return s.err
}

func featurePath(dir string) string { return filepath.Join(dir, FeatureFileName) }

func TestParseGateAcceptsOnAndOffCaseInsensitively(t *testing.T) {
	on, err := ParseGate("ON")
	require.NoError(t, err)
	require.True(t, on)

	off, err := ParseGate("off")
	require.NoError(t, err)
	require.False(t, off)

	_, err = ParseGate("yes")
	require.Error(t, err, "a typo must fail loudly rather than silently disabling the gate")
}

func TestApplySetsGPUPresentLabelWithMergePatch(t *testing.T) {
	p := &stubPatcher{}
	require.NoError(t, Apply(context.Background(), Config{
		NodeName:         "node-1",
		FeaturesDir:      t.TempDir(),
		PCIVendorEnabled: true,
		Patcher:          p,
	}))

	require.Equal(t, []string{"node-1"}, p.nodes)
	require.Len(t, p.patches, 1)
	require.JSONEq(t, `{"metadata":{"labels":{"nvidia.com/gpu.present":"true"}}}`, string(p.patches[0]))
}

func TestApplyWritesTheNFDFeatureFileWhenGateIsOn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Apply(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: true, Patcher: &stubPatcher{},
	}))

	// NFD's local source parses each line as key[=value]; this exact content is
	// what becomes feature.node.kubernetes.io/pci-10de.present=true.
	got, err := os.ReadFile(featurePath(dir))
	require.NoError(t, err)
	require.Equal(t, FeatureLine+"\n", string(got))
}

func TestApplyCreatesTheFeaturesDirWhenAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "features.d")
	require.NoError(t, Apply(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: true, Patcher: &stubPatcher{},
	}))
	require.FileExists(t, featurePath(dir))
}

func TestApplyRemovesAStaleFeatureFileWhenGateIsOff(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(featurePath(dir), []byte(FeatureLine+"\n"), 0o644))

	p := &stubPatcher{}
	require.NoError(t, Apply(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: false, Patcher: p,
	}))

	require.NoFileExists(t, featurePath(dir), "the gate converges either way")
	require.Len(t, p.patches, 1, "the gate covers only the feature file, never gpu.present")
}

func TestApplyWritesTheFeatureFileEvenWhenThePatchFails(t *testing.T) {
	dir := t.TempDir()
	err := Apply(context.Background(), Config{
		NodeName:         "node-1",
		FeaturesDir:      dir,
		PCIVendorEnabled: true,
		Patcher:          &stubPatcher{err: errors.New("nodes is forbidden")},
	})

	require.Error(t, err)
	require.FileExists(t, featurePath(dir), "an API failure must not cost us the label NFD derives")
}

func TestApplySkipsTheAPIWriteWithoutAPatcher(t *testing.T) {
	dir := t.TempDir()
	// A nil Patcher means the caller already reported why there is no client.
	require.NoError(t, Apply(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: true,
	}))
	require.FileExists(t, featurePath(dir))
}

func TestApplyErrorsWhenNodeNameIsEmpty(t *testing.T) {
	p := &stubPatcher{}
	err := Apply(context.Background(), Config{
		NodeName: "", FeaturesDir: t.TempDir(), PCIVendorEnabled: true, Patcher: p,
	})
	require.Error(t, err)
	require.Empty(t, p.patches, "patching an unnamed node would target nothing")
}

func TestRemoveClearsTheLabelWithANullMergePatch(t *testing.T) {
	p := &stubPatcher{}
	require.NoError(t, Remove(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: t.TempDir(), PCIVendorEnabled: true, Patcher: p,
	}))
	require.JSONEq(t, `{"metadata":{"labels":{"nvidia.com/gpu.present":null}}}`, string(p.patches[0]))
}

func TestRemoveDeletesTheFeatureFileOnlyWhenTheGateIsOn(t *testing.T) {
	t.Run("gate on removes it", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(featurePath(dir), []byte(FeatureLine+"\n"), 0o644))
		require.NoError(t, Remove(context.Background(), Config{
			NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: true, Patcher: &stubPatcher{},
		}))
		require.NoFileExists(t, featurePath(dir))
	})

	t.Run("gate off leaves a file we did not write", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(featurePath(dir), []byte("someone-else=true\n"), 0o644))
		require.NoError(t, Remove(context.Background(), Config{
			NodeName: "node-1", FeaturesDir: dir, PCIVendorEnabled: false, Patcher: &stubPatcher{},
		}))
		require.FileExists(t, featurePath(dir), "never delete an input this component did not supply")
	})
}

func TestRemoveIsIdempotentWhenTheFeatureFileIsAlreadyGone(t *testing.T) {
	require.NoError(t, Remove(context.Background(), Config{
		NodeName: "node-1", FeaturesDir: t.TempDir(), PCIVendorEnabled: true, Patcher: &stubPatcher{},
	}))
}
