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

package teardown

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedHostRoot builds the subset of the host tree setup.sh creates, as this
// container sees it (host root mounted at /host).
func seedHostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mock := filepath.Join(root, "var/lib/nvml-mock")
	require.NoError(t, os.MkdirAll(filepath.Join(mock, "driver/usr/lib64"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mock, "driver/usr/lib64/libnvidia-ml.so.1"), []byte("elf"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(mock, "ib"), 0o755))

	cdi := filepath.Join(root, "var/run/cdi")
	require.NoError(t, os.MkdirAll(cdi, 0o755))
	for _, name := range []string{"nvidia.yaml", "nvml-mock-nri.yaml", "other-vendor.yaml"} {
		require.NoError(t, os.WriteFile(filepath.Join(cdi, name), []byte("cdiVersion: \"0.6.0\"\n"), 0o644))
	}

	require.NoError(t, os.MkdirAll(filepath.Join(root, "run/nvidia"), 0o755))
	require.NoError(t, os.Symlink("/var/lib/nvml-mock/driver", filepath.Join(root, "run/nvidia/driver")))

	return root
}

func TestRunRemovesOurCDISpecsAndLeavesOthersAlone(t *testing.T) {
	root := seedHostRoot(t)
	require.NoError(t, Run(Config{HostRoot: root}, io.Discard))

	cdi := filepath.Join(root, "var/run/cdi")
	require.NoFileExists(t, filepath.Join(cdi, "nvidia.yaml"))
	require.NoFileExists(t, filepath.Join(cdi, "nvml-mock-nri.yaml"))
	require.FileExists(t, filepath.Join(cdi, "other-vendor.yaml"),
		"a spec another component staged is not ours to delete")
}

func TestRunEmptiesTheMockDirWithoutDeletingIt(t *testing.T) {
	root := seedHostRoot(t)
	require.NoError(t, Run(Config{HostRoot: root}, io.Discard))

	mock := filepath.Join(root, "var/lib/nvml-mock")
	// The directory is a DirectoryOrCreate hostPath mount point; removing it
	// would break the very mount this process is writing through.
	require.DirExists(t, mock)
	entries, err := os.ReadDir(mock)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRunRemovesTheGPUOperatorDriverSymlink(t *testing.T) {
	root := seedHostRoot(t)
	require.NoError(t, Run(Config{HostRoot: root}, io.Discard))

	_, err := os.Lstat(filepath.Join(root, "run/nvidia/driver"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRunLeavesARealDirectoryAtTheDriverPath(t *testing.T) {
	root := seedHostRoot(t)
	link := filepath.Join(root, "run/nvidia/driver")
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.MkdirAll(filepath.Join(link, "usr/bin"), 0o755))

	require.NoError(t, Run(Config{HostRoot: root}, io.Discard))
	require.DirExists(t, link, "a real driver root belongs to a real driver, not to us")
}

func TestRunIsIdempotentOnAnUntouchedRoot(t *testing.T) {
	require.NoError(t, Run(Config{HostRoot: t.TempDir()}, io.Discard))
}

func TestRunRefusesARootThatIsNotAbsolute(t *testing.T) {
	require.ErrorContains(t, Run(Config{HostRoot: ""}, io.Discard), "must be an absolute path")

	// Seed a tree reachable through a relative root, so a check that fires too
	// late — after the CDI specs have already been removed — fails here.
	t.Chdir(t.TempDir())
	spec := filepath.Join("host", "var/run/cdi", "nvidia.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(spec), 0o755))
	require.NoError(t, os.WriteFile(spec, []byte("x"), 0o644))

	require.ErrorContains(t, Run(Config{HostRoot: "host"}, io.Discard), "must be an absolute path")
	require.FileExists(t, spec, "a rejected root must stop every step, not just the wipe")
}

func TestRunReportsEachRemoval(t *testing.T) {
	root := seedHostRoot(t)
	var out strings.Builder
	require.NoError(t, Run(Config{HostRoot: root}, &out))
	require.Contains(t, out.String(), "nvml-mock-nri.yaml")
}
