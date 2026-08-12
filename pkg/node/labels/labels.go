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

// Package labels makes the two node labels nvml-mock causes to exist, exist —
// or stop existing.
//
// nvidia.com/gpu.present is written directly through the API server: it is an
// NVIDIA-namespaced key no NFD source derives.
//
// feature.node.kubernetes.io/pci-10de.present is NOT written directly. This
// package drops a feature file and lets NFD create the label, so that key has
// exactly one writer and "NFD works" stays distinguishable from "we set it
// ourselves" (#505).
//
// Why not NFD's PCI source? As deployed it cannot see our devices. (Upstream
// facts as of NFD v0.19.0, the version pinned in go.mod; the worker actually
// deployed comes from GPU Operator and may differ.) source/pci/utils.go
// resolves hostpath.SysfsDir.Path("bus/pci/devices"), and pkg/utils/hostpath
// builds SysfsDir from a private pathPrefix set only via linker -X. Upstream's
// container build passes HOSTMOUNT_PREFIX=/host- (Makefile), so the shipped
// worker reads /host-sys/bus/pci/devices — the real host /sys, hostPath-mounted
// by the worker DaemonSet. Our tree is at /var/lib/nvml-mock/sys and is
// reachable only through libpcimocksys.so, whose injection into the worker is
// inert twice over:
//
//	a) the shim rewrites only paths starting "/sys/" (k_prefixes[] in
//	   pkg/system/mockpcisysfs/c/shim.c), so "/host-sys/..." never matches.
//	b) nfd-worker is built -extldflags=-static, so it has no PT_INTERP and
//	   ignores LD_PRELOAD outright.
//
// The key itself is right: GPU Operator configures NFD with
// deviceLabelFields:[vendor] and whitelists class 0302, which is exactly the
// "pci-<vendor>.present" form, and our rendered devices carry all five
// attributes NFD treats as mandatory. Only visibility is missing.
//
// NFD's *local* source has no such limit: source/local/local.go reads
// featureFilesDir, a plain literal (not host-prefixed), and the worker mounts
// that host directory at the same path. Each line parses as key[=value]. That
// is the supported route, and it needs no node RBAC.
package labels

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// GPUPresentLabel is the one label this component writes directly.
	GPUPresentLabel = "nvidia.com/gpu.present"

	// FeatureFileName is the file this package drops in NFD's features.d.
	FeatureFileName = "nvml-mock.features"

	// FeatureLine is that file's only line, in the key=value form NFD's local
	// source parses, and yields
	// feature.node.kubernetes.io/pci-10de.present=true.
	FeatureLine = "pci-10de.present=true"

	setPatch    = `{"metadata":{"labels":{"nvidia.com/gpu.present":"true"}}}`
	removePatch = `{"metadata":{"labels":{"nvidia.com/gpu.present":null}}}`
)

// NodePatcher applies a raw JSON merge patch to one node. Deliberately one
// method wide: it keeps client-go out of this package entirely, so the tests
// need no generated fake clientset in vendor/.
type NodePatcher interface {
	Patch(ctx context.Context, nodeName string, patch []byte) error
}

// Config describes the node this process runs on and where its inputs live. A
// nil Patcher skips the API write, for callers that have already reported why
// no client could be built.
type Config struct {
	NodeName         string
	FeaturesDir      string
	PCIVendorEnabled bool
	Patcher          NodePatcher
}

// ParseGate reads the MOCK_NFD_PCI_LABEL contract: "on" or "off",
// case-insensitive. Anything else is a typo, and the caller is expected to
// treat that as fatal rather than silently disabling the gate.
func ParseGate(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid pci-vendor-label %q: expected on or off", v)
	}
}

// Apply makes both labels exist.
//
// It is deliberately not split the way RemoveFeatureFile and RemoveLabel are:
// at setup time both halves are tolerant, so there is no severity to separate,
// and keeping them in one call keeps the ordering below where it is enforced.
//
// The argument order is load-bearing, not stylistic: Go evaluates call arguments
// left to right, so the feature file is converged before nvidia.com/gpu.present
// can be observed on the node. tests/e2e/go/scenario_nfd_test.go waits for
// gpu.present as a synchronisation barrier and then asserts pci-10de.present is
// absent, to prove NFD is that label's only writer; if the patch landed first
// the barrier would let the negative check pass vacuously, and only an
// intermittent Kind run would notice, so a unit test in this package pins the
// order too.
func Apply(ctx context.Context, cfg Config) error {
	return errors.Join(convergeFeatureFile(cfg), patch(ctx, cfg, setPatch))
}

// RemoveFeatureFile unwinds the write Apply made, only when the gate is on, so
// this component never deletes an input it did not supply; NFD retires the label
// it owns on its next scan once the file is gone.
//
// Split from RemoveLabel because the two failures differ in severity: a file
// left behind keeps NFD asserting pci-10de.present on a node with no mock, while
// a lingering gpu.present is cosmetic. Callers grade exit codes on that.
func RemoveFeatureFile(cfg Config) error {
	if !cfg.PCIVendorEnabled {
		return nil
	}
	return removeFeatureFile(cfg)
}

// RemoveLabel clears nvidia.com/gpu.present. Callers run it after every
// filesystem step, because a hook killed mid-run should lose only this.
func RemoveLabel(ctx context.Context, cfg Config) error {
	return patch(ctx, cfg, removePatch)
}

// convergeFeatureFile writes the file when the gate is on and removes any file
// an earlier run left behind when it is off, so either arm converges.
func convergeFeatureFile(cfg Config) error {
	if !cfg.PCIVendorEnabled {
		return removeFeatureFile(cfg)
	}
	if err := os.MkdirAll(cfg.FeaturesDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", cfg.FeaturesDir, err)
	}
	path := filepath.Join(cfg.FeaturesDir, FeatureFileName)
	if err := os.WriteFile(path, []byte(FeatureLine+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func removeFeatureFile(cfg Config) error {
	path := filepath.Join(cfg.FeaturesDir, FeatureFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func patch(ctx context.Context, cfg Config, body string) error {
	if cfg.Patcher == nil {
		return nil
	}
	if cfg.NodeName == "" {
		return errors.New("node name is empty: refusing to patch an unnamed node")
	}
	if err := cfg.Patcher.Patch(ctx, cfg.NodeName, []byte(body)); err != nil {
		return fmt.Errorf("patching node %s label %s: %w", cfg.NodeName, GPUPresentLabel, err)
	}
	return nil
}
