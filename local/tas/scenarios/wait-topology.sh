#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION
# SPDX-License-Identifier: Apache-2.0
#
# Block until Slurm's block topology groups the nodes exactly as their
# nvidia.com/gpu.clique labels do, then print it.
#
# The topology reaches slurmctld in two hops that both land after the slurmd
# pods are Ready: topograph rewrites topology.conf, which the reconfigure
# sidecar applies, and annotates each Node, which the slurm-operator copies
# onto the Slurm node. Until both have happened Slurm runs on the seed block,
# which holds no node.
#
# Usage: wait-topology.sh [namespace] [timeout-seconds]
set -euo pipefail

NAMESPACE="${1:-slurm}"
TIMEOUT_S="${2:-600}"
# The nodes slurmd runs on; local/slinky/slurm.values.yaml selects them.
NODE_SELECTOR="${NODE_SELECTOR:-mokka.nvidia.com/type=sgpu}"

slurm() {
  kubectl -n "$NAMESPACE" exec slurm-controller-0 -c slurmctld -- "$@"
}

# One line per clique: its node names, sorted and comma-joined.
cliques() {
  kubectl get nodes -l "$NODE_SELECTOR" \
    -o jsonpath='{range .items[*]}{.metadata.labels.nvidia\.com/gpu\.clique}|{.metadata.name}{"\n"}{end}' |
    awk -F'|' '$1 != "" { g[$1] = ($1 in g) ? g[$1] "," $2 : $2 } END { for (k in g) print g[k] }' |
    while read -r group; do tr ',' '\n' <<<"$group" | sort | paste -sd, -; done | sort
}

# One line per Slurm base block that holds nodes, in the same form.
# Aggregated levels print as AggregatedBlock= and are skipped.
blocks() {
  slurm scontrol show topology |
    awk '/^BlockName=/ { for (i = 1; i <= NF; i++) if ($i ~ /^Nodes=./) print substr($i, 7) }' |
    while read -r list; do slurm scontrol show hostnames "$list" | sort | paste -sd, -; done | sort
}

expected=$(cliques)
if (( $(grep -c . <<<"$expected") < 2 )); then
  echo "nodes matching $NODE_SELECTOR form fewer than two cliques (${expected:-none});" \
    "is this the tas cluster (make cluster-create PROFILE=tas)?" >&2
  exit 1
fi

deadline=$(( $(date +%s) + TIMEOUT_S ))
while :; do
  engine=$(kubectl -n "$NAMESPACE" get configmap slurm-config-extra \
    -o jsonpath='{.metadata.annotations.topograph\.run/engine}' 2>/dev/null || true)
  actual=$(blocks 2>/dev/null || true)
  if [[ "$engine" == "slinky" && "$actual" == "$expected" ]]; then
    break
  fi
  if (( $(date +%s) >= deadline )); then
    {
      echo "timed out after ${TIMEOUT_S}s waiting for Slurm blocks to match the cliques"
      echo "cliques:"; sed 's/^/  /' <<<"$expected"
      echo "Slurm blocks with nodes:"; sed 's/^/  /' <<<"${actual:-<none>}"
      echo "slurm-config-extra written by: ${engine:-<not topograph>}"
      kubectl get nodes -l "$NODE_SELECTOR" \
        -o custom-columns='NODE:.metadata.name,SPEC:.metadata.annotations.topology\.slinky\.slurm\.net/spec'
    } >&2
    exit 1
  fi
  sleep 5
done

echo "Slurm blocks match the NVLink cliques:"
slurm scontrol show topology
