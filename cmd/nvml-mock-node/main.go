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

// nvml-mock-node performs the two cluster-facing steps of the nvml-mock
// DaemonSet's lifecycle: `label` makes the node labels the mock causes to exist
// exist (called from setup.sh), and `teardown` unwinds the node-local state
// (the preStop hook). It exists so the image ships no kubectl binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/NVIDIA/k8s-test-infra/pkg/node/labels"
	"github.com/NVIDIA/k8s-test-infra/pkg/node/teardown"
)

const (
	// defaultHostRoot is where the chart mounts the host filesystem.
	defaultHostRoot = "/host"

	// featuresDirRel is the container mountPath the chart pins for NFD's
	// features.d, independent of which host directory nodeLabels.featuresDir
	// selects.
	featuresDirRel = "etc/kubernetes/node-feature-discovery/features.d"

	// The two timeouts follow the pod lifecycle: `label` runs at startup with no
	// deadline pressure, while `teardown` must finish inside a
	// terminationGracePeriodSeconds of 1-2s or be SIGKILLed mid-run.
	labelAPITimeout    = 10 * time.Second
	teardownAPITimeout = time.Second
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// Writes to this CLI's stdout/stderr can't meaningfully fail-recover, so these
// helpers swallow the error (and satisfy errcheck, which only whitelists the
// literal os.Stdout/os.Stderr destinations, not io.Writer parameters).
func fprint(w io.Writer, a ...any)                 { _, _ = fmt.Fprint(w, a...) }
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func usage(w io.Writer) {
	fprint(w, `usage: nvml-mock-node <command> [flags]

commands:
  label      make nvidia.com/gpu.present exist and converge the NFD pci-10de
             feature file (called from setup.sh)
  teardown   remove the node-local mock state and both labels (preStop hook)

flags:
  --node-name         node to patch (default $NODE_NAME)
  --pci-vendor-label  on|off: write the NFD pci-10de feature file (default $MOCK_NFD_PCI_LABEL, else on)
  --host-root         host filesystem root as mounted here (default /host)
  --features-dir      NFD features.d directory (default <host-root>/`+featuresDirRel+`)
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(args []string, stdout, stderr io.Writer) int {
	var nodeName, pciVendorLabel, hostRoot, featuresDir string
	fs := flag.NewFlagSet("nvml-mock-node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // printed explicitly below so it isn't duplicated
	// Every flag falls back to the environment the DaemonSet already sets: a
	// preStop hook's exec.command is an argv array run without a shell, so the
	// chart cannot interpolate $NODE_NAME into it.
	fs.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "node to patch")
	fs.StringVar(&pciVendorLabel, "pci-vendor-label", envOr("MOCK_NFD_PCI_LABEL", "on"), "on|off")
	fs.StringVar(&hostRoot, "host-root", defaultHostRoot, "host filesystem root as mounted here")
	fs.StringVar(&featuresDir, "features-dir", "", "NFD features.d directory")

	// Flags may appear before or after the subcommand. The stdlib flag package
	// stops at the first non-flag token, so parse in a loop and take the first
	// bare token as the command: both the usage text and setup.sh put the
	// command first, and nvml-mock-ctl parses the same way.
	var cmd string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				usage(stdout)
				return 0
			}
			usage(stderr)
			return 2
		}
		remaining := fs.Args()
		if len(remaining) == 0 {
			break
		}
		if cmd != "" {
			fprintf(stderr, "unexpected argument %q\n", remaining[0])
			usage(stderr)
			return 2
		}
		cmd = remaining[0]
		rest = remaining[1:]
	}
	if cmd == "" {
		usage(stderr)
		return 2
	}
	if featuresDir == "" {
		featuresDir = filepath.Join(hostRoot, featuresDirRel)
	}

	switch cmd {
	case "help":
		usage(stdout)
		return 0
	case "label", "teardown":
	default:
		fprintf(stderr, "unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}

	gate, err := labels.ParseGate(pciVendorLabel)
	if err != nil {
		fprintf(stderr, "ERROR: %v\n", err)
		return 2
	}
	cfg := labels.Config{NodeName: nodeName, FeaturesDir: featuresDir, PCIVendorEnabled: gate}

	if cmd == "label" {
		return doLabel(cfg, stdout, stderr)
	}
	return doTeardown(cfg, hostRoot, stdout, stderr)
}

// patcherOrWarn degrades to a nil NodePatcher, which labels.Apply/Remove skip.
// A cluster without the RBAC, or a container run outside Kubernetes, then
// behaves as it did when the image shipped no kubectl.
func patcherOrWarn(cfg labels.Config, action string, stderr io.Writer) labels.NodePatcher {
	p, err := labels.NewNodePatcher()
	if err != nil {
		fprintf(stderr, "WARNING: no Kubernetes client (%v); %s not %s on node %q\n",
			err, labels.GPUPresentLabel, action, cfg.NodeName)
		return nil
	}
	return p
}

func doLabel(cfg labels.Config, stdout, stderr io.Writer) int {
	cfg.Patcher = patcherOrWarn(cfg, "written", stderr)
	ctx, cancel := context.WithTimeout(context.Background(), labelAPITimeout)
	defer cancel()

	// Tolerant on purpose: an optional label must not abort the entrypoint
	// before setup.sh's remaining steps, so this warns and exits 0.
	if err := labels.Apply(ctx, cfg); err != nil {
		fprintf(stderr, "WARNING: %v\n", err)
		return 0
	}
	fprintf(stdout, "node labels applied on %s\n", cfg.NodeName)
	return 0
}

func doTeardown(cfg labels.Config, hostRoot string, stdout, stderr io.Writer) int {
	// Filesystem first, API last: see teardown.Run's ordering rationale.
	fsErr := teardown.Run(teardown.Config{HostRoot: hostRoot}, stdout)

	cfg.Patcher = patcherOrWarn(cfg, "removed", stderr)
	ctx, cancel := context.WithTimeout(context.Background(), teardownAPITimeout)
	defer cancel()
	labelErr := labels.Remove(ctx, cfg)

	// Report rather than swallow: a non-zero exit surfaces as a
	// FailedPreStopHook event instead of vanishing.
	if err := errors.Join(fsErr, labelErr); err != nil {
		fprintf(stderr, "teardown incomplete on %s: %v\n", cfg.NodeName, err)
		return 1
	}
	fprintf(stdout, "mock GPU environment cleaned up on %s\n", cfg.NodeName)
	return 0
}
