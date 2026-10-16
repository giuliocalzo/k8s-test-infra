#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION
# SPDX-License-Identifier: Apache-2.0
#
# Gang-scheduling check: Slurm gives a two-node job both nodes of one block at
# once, or holds it, and never splits it across blocks.
#
# Asserts three things. A topology-unaware Slurm fails the last two, since it
# takes the lowest-numbered free nodes:
#   1. gang: two 2-node jobs run at the same time, each on a whole block.
#   2. fragmentation: with one node of block A held, a 2-node job takes block
#      B whole rather than A's free node plus one of B's.
#   3. all-or-nothing: with one node held in each block, a 2-node job stays
#      pending although two nodes are idle.
#
# Reads the blocks from Slurm, so it checks that the scheduler honours its
# topology; local/tas/scenarios/wait-topology.sh checks that the topology is
# the cliques.

set -euo pipefail

# _NAMESPACE in local/tas/tas.tiltfile.
NAMESPACE="${NAMESPACE:-slurm}"
# How long a job may take to start, or the cluster to drain.
SETTLE_S="${SETTLE_S:-90}"
# How long a job must stay pending to count as held: longer than Slurm's 30s
# backfill interval, so the backfill scheduler has had a pass at it too.
PENDING_S="${PENDING_S:-45}"
JOB_NAME=tas-gang

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

slurm() {
  kubectl -n "${NAMESPACE}" exec slurm-controller-0 -c slurmctld -- "$@"
}

# expand <hostlist>: comma-joined, sorted node names.
expand() {
  slurm scontrol show hostnames "$1" | sort | paste -sd, -
}

# One line per base block that holds nodes.
blocks() {
  slurm scontrol show topology |
    awk '/^BlockName=/ { for (i = 1; i <= NF; i++) if ($i ~ /^Nodes=./) print substr($i, 7) }' |
    while read -r list; do expand "${list}"; done | sort
}

# hold <sbatch args...>: submit a whole-node sleeper and print its job id.
hold() {
  local id
  id=$(slurm sbatch --parsable -J "${JOB_NAME}" --exclusive "$@" --wrap 'sleep 3600') \
    || fail "sbatch $* was rejected"
  printf '%s\n' "${id%%;*}"
}

# job <id>: "<state> <nodes>", nodes empty while it has none.
job() {
  local state list nodes=""
  read -r state list <<<"$(slurm squeue -h -j "$1" -o '%T %N')" || true
  if [[ -n "${list:-}" ]]; then
    nodes=$(expand "${list}")
  fi
  printf '%s %s\n' "${state:-GONE}" "${nodes}"
}

# wait_running <id>...: block until every job is RUNNING.
wait_running() {
  local deadline=$(( $(date +%s) + SETTLE_S )) id state
  for id in "$@"; do
    while :; do
      read -r state _ <<<"$(job "${id}")"
      [[ "${state}" == RUNNING ]] && break
      (( $(date +%s) < deadline )) \
        || fail "job ${id} is ${state} after ${SETTLE_S}s, not RUNNING: $(slurm squeue -h -j "${id}" -o 'reason=%r')"
      sleep 2
    done
  done
}

# reset: cancel this script's jobs and wait for every node to be idle, so one
# check's leftovers cannot hold nodes the next check counts as free.
reset() {
  slurm scancel -n "${JOB_NAME}"
  local deadline=$(( $(date +%s) + SETTLE_S ))
  until [[ -z "$(slurm squeue -h -n "${JOB_NAME}" -o '%i')" &&
           -z "$(slurm sinfo -h -N -o '%T' | grep -vx idle || true)" ]]; do
    (( $(date +%s) < deadline )) \
      || fail "nodes not idle ${SETTLE_S}s after cancelling: $(slurm sinfo -h -N -o '%N=%T' | paste -sd' ' -)"
    sleep 2
  done
}
trap reset EXIT

printf '==> reading Slurm blocks\n'
all_blocks=$(blocks)
read -r block_a block_b extra <<<"$(paste -sd' ' - <<<"${all_blocks}")" || true
[[ -n "${block_b:-}" && -z "${extra:-}" ]] \
  || fail "expected two blocks with nodes, got: ${all_blocks:-none}; did tas-topology pass?"
IFS=, read -r a1 a2 a_more <<<"${block_a}"
IFS=, read -r b1 b2 b_more <<<"${block_b}"
[[ -n "${a2:-}" && -z "${a_more:-}" && -n "${b2:-}" && -z "${b_more:-}" ]] \
  || fail "expected two nodes per block, got ${block_a} and ${block_b}"
printf 'OK: block A is %s, block B is %s\n' "${block_a}" "${block_b}"
reset

printf '==> gang: two 2-node jobs at once, one whole block each\n'
j1=$(hold -N2)
j2=$(hold -N2)
wait_running "${j1}" "${j2}"
read -r _ n1 <<<"$(job "${j1}")"
read -r _ n2 <<<"$(job "${j2}")"
placed=$(printf '%s\n%s\n' "${n1}" "${n2}" | sort | paste -sd' ' -)
[[ "${placed}" == "${block_a} ${block_b}" ]] \
  || fail "the 2-node jobs ran on ${n1} and ${n2}, not one whole block each (${block_a} / ${block_b})"
printf 'OK: job %s on %s and job %s on %s, both running\n' "${j1}" "${n1}" "${j2}" "${n2}"
reset

printf '==> fragmentation: with %s held, a 2-node job takes block B whole\n' "${a1}"
held=$(hold -N1 -w "${a1}")
wait_running "${held}"
j=$(hold -N2)
wait_running "${j}"
read -r _ n <<<"$(job "${j}")"
[[ "${n}" == "${block_b}" ]] \
  || fail "with ${a1} held the 2-node job ran on ${n}, not block B (${block_b})"
printf 'OK: job %s on %s; %s was left idle\n' "${j}" "${n}" "${a2}"
reset

printf '==> all-or-nothing: with %s and %s held, a 2-node job waits\n' "${a1}" "${b1}"
held_a=$(hold -N1 -w "${a1}")
held_b=$(hold -N1 -w "${b1}")
wait_running "${held_a}" "${held_b}"
j=$(hold -N2)
sleep "${PENDING_S}"
read -r state n <<<"$(job "${j}")"
[[ "${state}" == PENDING && -z "${n}" ]] \
  || fail "with one node held per block the 2-node job is ${state} on '${n}', expected PENDING with no nodes"
# Two idle nodes are enough for the job, so any other reason (a limit, a
# drained node) means the topology constraint was never what held it.
reason=$(slurm squeue -h -j "${j}" -o '%r')
[[ "${reason}" == *[Tt]opology* ]] \
  || fail "the 2-node job is PENDING for '${reason}', not for want of a whole block"
printf 'OK: job %s still PENDING (%s) after %ss with %s and %s idle\n' \
  "${j}" "${reason}" "${PENDING_S}" "${a2}" "${b2}"

printf '\n==> tas-gang passed: Slurm placed every 2-node job inside one block or held it.\n'
