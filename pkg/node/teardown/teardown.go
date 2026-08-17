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

// Package teardown removes the node-local state setup.sh created. It is the
// body of the nvml-mock DaemonSet's preStop hook.
package teardown

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Paths are relative to Config.HostRoot, which is where this container sees
// the host filesystem (/host under the chart's volumeMounts).
const (
	mockDirRel       = "var/lib/nvml-mock"
	driverSymlinkRel = "run/nvidia/driver"
	cdiDirRel        = "var/run/cdi"
)

// ErrInvalidHostRoot reports that Run rejected its configuration and ran no
// step at all. Callers that pair Run with their own filesystem cleanup match on
// it to skip that work too, rather than resolving the same bad root themselves.
var ErrInvalidHostRoot = errors.New("invalid host root")

// cdiSpecs are the two specs setup.sh stages. Both name device nodes under the
// mock directory, so leaving either behind after the wipe below hands the
// runtime a spec whose hostPaths no longer exist: containerd fails container
// creation with "failed to stat CDI host device" and the kubelet retries it
// forever.
var cdiSpecs = []string{"nvidia.yaml", "nvml-mock-nri.yaml"}

// Config selects the tree to tear down.
type Config struct {
	HostRoot string
}

// Progress messages can't meaningfully fail-recover, so this swallows the write
// error (and satisfies errcheck, which whitelists only the literal
// os.Stdout/os.Stderr destinations, not io.Writer parameters). Same helper shape
// as cmd/nvml-mock-ctl.
func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// Run removes every piece of node-local state nvml-mock owns, continuing past
// individual failures and joining them, so one unwritable path cannot hide the
// rest of the teardown. The one exception precedes all of them: an unusable
// HostRoot returns ErrInvalidHostRoot before any step runs.
//
// Order matters. The CDI specs go first, ahead of the device nodes they name,
// which closes the window where a spec references deleted paths. The API-side
// label removal is NOT done here; callers do it after this returns, because
// terminationGracePeriodSeconds is 1-2s in practice and a hook killed mid-run
// should lose only the cosmetic label, never this filesystem state.
//
// /run/nvidia/validations/toolkit-ready is deliberately untouched: its owner is
// GPU Operator's nvidia-validator, which deletes and rewrites it on every run.
// Removing another component's state from a hook we own would be wrong even
// though it looks adjacent.
func Run(cfg Config, out io.Writer) error {
	// Validated once, up front: a rejected root must stop every step, not just
	// the one that happens to check it.
	if err := validateHostRoot(cfg.HostRoot); err != nil {
		return err
	}
	return errors.Join(
		removeCDISpecs(cfg.HostRoot, out),
		wipeMockDir(cfg.HostRoot, out),
		removeDriverSymlink(cfg.HostRoot, out),
	)
}

func removeCDISpecs(root string, out io.Writer) error {
	var errs []error
	for _, name := range cdiSpecs {
		path := filepath.Join(root, cdiDirRel, name)
		err := os.Remove(path)
		switch {
		case err == nil:
			logf(out, "CDI spec removed: %s\n", path)
		case errors.Is(err, fs.ErrNotExist):
		default:
			errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// validateHostRoot rejects a root the recursive delete below must not run
// against. It defends against a misconfigured or empty --host-root, not against
// a hostile one: anything relative would resolve against this process's working
// directory rather than the mounted host tree. A caller able to choose argv here
// already has a privileged container with the host filesystem mounted.
func validateHostRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("refusing to tear down %q: %w: must be an absolute path", root, ErrInvalidHostRoot)
	}
	return nil
}

// wipeMockDir empties the mock directory without removing it: the directory is
// a DirectoryOrCreate hostPath mount point this process writes through.
func wipeMockDir(root string, out io.Writer) error {
	dir := filepath.Join(root, mockDirRel)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
		}
	}
	if len(errs) == 0 {
		logf(out, "mock GPU environment removed from %s\n", dir)
	}
	return errors.Join(errs...)
}

func removeDriverSymlink(root string, out io.Writer) error {
	path := filepath.Join(root, driverSymlinkRel)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	// Only ever remove the compatibility symlink setup.sh created. A real
	// directory here means a real driver root, which is not ours to delete.
	if info.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	logf(out, "GPU Operator driver symlink removed: %s\n", path)
	return nil
}
