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
// DaemonSet's lifecycle: `label` makes both node labels the mock is responsible
// for exist (called from setup.sh), and `teardown` unwinds the node-local state
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

//nolint:cyclop // mechanical arg parsing and subcommand dispatch
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

	// The gate is parsed per subcommand, not here: `label` treats a typo as
	// fatal, while `teardown` must still unwind the filesystem state a bad value
	// has no bearing on.
	cfg := labels.Config{NodeName: nodeName, FeaturesDir: featuresDir}

	if cmd == "label" {
		return doLabel(cfg, pciVendorLabel, stdout, stderr)
	}
	return doTeardown(cfg, pciVendorLabel, hostRoot, stdout, stderr)
}

// onNode names the node in a message only when a name is known. Both the flag
// and $NODE_NAME can be empty — the feature file needs no node identity — and
// an unconditional %q there reads as `on node ""`.
func onNode(name string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf(" on node %q", name)
}

// patcherOrWarn degrades to a nil NodePatcher, which labels.Apply and
// labels.RemoveLabel skip. Only a missing in-cluster config is caught here — a
// container run outside Kubernetes — and everything that does not need the API
// server still happens. An RBAC denial is invisible until patch time, which
// `label` tolerates and `teardown` reports as a warning.
func patcherOrWarn(cfg labels.Config, action string, stderr io.Writer) labels.NodePatcher {
	p, err := labels.NewNodePatcher()
	if err != nil {
		fprintf(stderr, "WARNING: no Kubernetes client (%v); %s not %s%s\n",
			err, labels.GPUPresentLabel, action, onNode(cfg.NodeName))
		return nil
	}
	return p
}

func doLabel(cfg labels.Config, pciVendorLabel string, stdout, stderr io.Writer) int {
	// The one fatal use of the gate: at setup time a typo must stop the pod
	// rather than silently leave the NFD feature file unwritten.
	gate, err := labels.ParseGate(pciVendorLabel)
	if err != nil {
		fprintf(stderr, "ERROR: %v\n", err)
		return 2
	}
	cfg.PCIVendorEnabled = gate

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

func doTeardown(cfg labels.Config, pciVendorLabel, hostRoot string, stdout, stderr io.Writer) int {
	// An unparseable gate must not cost us the teardown: it governs the NFD
	// feature file alone, while the CDI specs, the host overlay and the driver
	// symlink below are staged unconditionally. Gate-off is the conservative
	// arm — it leaves behind a feature file this component may not have written,
	// the same choice RemoveFeatureFile makes for an explicit "off".
	gate, err := labels.ParseGate(pciVendorLabel)
	if err != nil {
		fprintf(stderr, "WARNING: %v; leaving the NFD feature file in place\n", err)
	}
	cfg.PCIVendorEnabled = gate

	// Filesystem first, API last: see teardown.Run's ordering rationale.
	fsErr := errors.Join(
		teardown.Run(teardown.Config{HostRoot: hostRoot}, stdout),
		labels.RemoveFeatureFile(cfg),
	)

	cfg.Patcher = patcherOrWarn(cfg, "removed", stderr)
	ctx, cancel := context.WithTimeout(context.Background(), teardownAPITimeout)
	defer cancel()
	labelErr := labels.RemoveLabel(ctx, cfg)

	// Only state left on the node is worth failing the hook for. The label
	// patch is the likeliest step to fail routinely — teardownAPITimeout has to
	// cover a TLS handshake plus a PATCH inside a terminationGracePeriodSeconds
	// of 1 — and routine FailedPreStopHook events would drown out the
	// filesystem failures the event exists to surface.
	if labelErr != nil {
		fprintf(stderr, "WARNING: %v; %s may be left behind%s\n",
			labelErr, labels.GPUPresentLabel, onNode(cfg.NodeName))
	}
	if fsErr != nil {
		fprintf(stderr, "teardown incomplete%s: %v\n", onNode(cfg.NodeName), fsErr)
		return 1
	}
	fprintf(stdout, "mock GPU environment cleaned up on %s\n", cfg.NodeName)
	return 0
}
