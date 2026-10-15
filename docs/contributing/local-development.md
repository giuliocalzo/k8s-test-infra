# Local Development

Mokka's local loop is a Kind cluster plus [Tilt](https://tilt.dev/). Tilt builds
the images, installs the charts, and redeploys on save; you create the cluster
once and leave it running.

```bash
make cluster-create          # once
tilt up -- --gpu-operator    # the dev loop
```

## Prerequisites

`docker`, [`kind`](https://kind.sigs.k8s.io/), [`tilt`](https://tilt.dev/),
`helm`, `kubectl`, and a Go toolchain matching `go.mod`.

## Step 1 — create a cluster

Three cluster shapes exist, selected with `PROFILE`:

| `PROFILE`        | Shape                                                    | Cluster name           | Use with                                 |
|------------------|----------------------------------------------------------|------------------------|------------------------------------------|
| `default`        | 1 control-plane + 2 workers                              | `mokka`                | everything except compute-domain and tas |
| `compute-domain` | 1 control-plane + 4 workers, one NVLink clique each      | `mokka-compute-domain` | `--compute-domain`, `--topograph`        |
| `tas`            | 1 control-plane + 4 workers in two NVLink cliques of two | `mokka-tas`            | `--tas`                                  |

```bash
make cluster-create                         # default
make cluster-create PROFILE=compute-domain
make cluster-create PROFILE=tas

make cluster-delete                         # PROFILE must match creation
```

The default shape serves both a single release and a per-worker fleet, so
switching between them needs no rebuild.

> [!NOTE]
> **The control-plane node is deliberately excluded**
>
> `local/nvml-mock.values.yaml` pins the DaemonSet to nodes labelled
> `mokka.nvidia.com/type=sgpu`. Without that the mock lands on the control
> plane too — it tolerates every taint — while the GPU Operator, FGO and NFD
> operands stop at the `NoSchedule` taint. You would get a node advertising a
> driver nobody consumes.

## Step 2 — start Tilt

`tilt up` with no flags installs one nvml-mock release across every worker using
the chart's [default profile](../helm-chart.md#when-to-use-each-profile).
Everything else is a flag.

### Choosing what the fleet looks like

| Flag | Effect |
|---|---|
| `--gpu-profile <name>` | Which GPU profile the release uses, instead of the chart's default. One of `a100`, `h100`, `b200`, `gb200`, `gb300`, `l40s`, `t4` |
| `--multi-gpu-profile` | One release per worker instead of one for the fleet: `a100` on `worker-0`, `t4` on `worker-1`. Ignores `--gpu-profile` |
| `--compute-domain` | GB200 profile with an NVLink topology overlay. Needs the `compute-domain` cluster |
| `--nvmlmock-image <ref>` | Skip the image build and pin the release to an image you already built, as CI does |

### Adding consumers

| Flag | Deploys |
|---|---|
| `--gpu-operator` | The NVIDIA GPU Operator, in CDI mode |
| `--dra` | The NVIDIA DRA driver |
| `--fgo` | Run:ai's fake-gpu-operator alongside Mokka, splitting workers into two pools |
| `--topograph` | [topograph](https://github.com/NVIDIA/topograph) network-topology discovery |
| `--observability` | kube-prometheus-stack and a Grafana dashboard for the mock fleet |
| `--dynamo` | The NVIDIA Dynamo operator and a `qwen3` inference graph whose mocker worker runs on a mock GPU |
| `--slinky` | cert-manager, the Slinky slurm-operator and a Slurm cluster with one GPU `slurmd` per mock-GPU worker |
| `--tas` | topograph feeding the NVLink cliques to Slurm as block topology, so a multi-node job runs inside one clique or waits. Needs the `tas` cluster |
| `--control-plane` | The Mokka control-plane image and the `mokka-crds` chart |

### What composes and what does not

Most flags stack. These do not:

| Combination | Why |
|---|---|
| `--fgo` with `--gpu-operator` | FGO replaces the GPU Operator |
| `--fgo` with `--compute-domain` | Different fleet shapes |
| `--compute-domain` with `--multi-gpu-profile` or `--gpu-profile` | The compute-domain scenario fixes both |
| `--compute-domain` with `--nvmlmock-image` | The scenario builds its own layered images |
| `--slinky` with `--dynamo` | `slurmd` claims every mock GPU, so the Dynamo worker could never schedule |

Five flags imply others: `--topograph` implies `--compute-domain`, because
cliques only exist there; `--observability` implies `--gpu-operator`, because
`dcgm-exporter` is one of its operands; `--dynamo` and `--slinky` imply
`--gpu-operator`, because their workloads request `nvidia.com/gpu`; `--tas`
implies `--topograph` and `--slinky`, the two parts it connects.

```bash
tilt up -- --gpu-profile gb200
tilt up -- --multi-gpu-profile --gpu-operator
tilt up -- --gpu-operator --dra
tilt up -- --observability
tilt up -- --dynamo
tilt up -- --slinky

make cluster-create PROFILE=compute-domain
tilt up -- --compute-domain --dra

make cluster-create PROFILE=tas
tilt up -- --tas
```

## Manual triggers in the Tilt UI

Some scenarios add buttons rather than running automatically.

**`compute-domain-tests`** — `check-fabric` asserts the topology overlay gave
each node the expected clique and cluster UUID; `topology-rebind` live-rebinds
the topology and re-asserts.

**`observability-tests`** — `inject-thermal` pins one GPU's temperature and
asserts Prometheus serves exactly that value while its siblings keep varying;
`inject-xid` trips an uncorrectable ECC fault and asserts the Xid reaches
`DCGM_FI_DEV_XID_ERRORS`. Both inject with `nvml-mock-ctl` rather than
`helm upgrade`, which would recycle the exporter and tear a hole in the series,
and both **fail if the fault does not reach Prometheus** — the scrape path is
asserted, not eyeballed.

Grafana is port-forwarded to <http://localhost:3000/d/mokka-gpu> (`admin` /
`mokka`) once its resource is ready.

**`dynamo-tests`** — `dynamo-smoke` asserts the `qwen3` worker sees exactly
one GPU whose name matches its node's `nvidia.com/gpu.product` label, then
sends a chat completion through the frontend and checks the token count. The
frontend is port-forwarded to <http://localhost:8000/v1/models>. The
[NVIDIA Dynamo guide](../guides/dynamo/README.md) walks through the same setup
from published artifacts.

**`slinky-tests`** — `slurm-smoke` asserts every Slurm node is idle with the
GPUs it was configured with, that a 2-GPU job is given exactly 2 and they are
the node's profile, and that a job using every GPU on every node runs. The
[Slinky guide](../guides/slinky/README.md) walks through the same setup from
published artifacts.

**`tas-tests`** — `tas-gang` reads the blocks from Slurm and asserts three
placements: two 2-node jobs run at once on one whole block each; with one node
of a block busy, a 2-node job takes the other block rather than pairing nodes
across blocks; with one node busy in each block, a 2-node job waits with reason
`No suitable topology unit found`, although two nodes are idle. How topograph
hands the cliques to Slurm is in
[local/README.md](../../local/README.md#topology-aware-scheduling---tas).

## Overriding Helm values

Values layer in this order, last wins:

| Source | Committed |
|---|---|
| Chart defaults | — |
| `local/nvml-mock.values.yaml` — shared local baseline | yes |
| `local/<consumer>/nvml-mock.values.yaml` — per-consumer tweaks | yes |
| `local/nvml-mock.values.local.yaml` — your machine | **no**, gitignored |
| Tilt flags that map to a chart value: `--gpu-profile`, `--nvmlmock-image`, `--control-plane` | — |

A flag reaches Helm as `--set`, which beats every file. So `gpu.profile` in a
values file takes effect only when `--gpu-profile` is not passed.

```bash
cat > local/nvml-mock.values.local.yaml <<'EOF'
gpu:
  count: 2
infiniband:
  enabled: true
  mockTier: full
EOF
```

Tilt does not watch values files, and the Tiltfile looks for the personal file only
when it loads. Create it before `tilt up`; after editing any values file, trigger
the `nvml-mock` resource in the Tilt UI to re-apply it.

## Related

| To read about | See |
|---|---|
| Running the tests | [Testing](testing.md) |
| What a profile defines | [Configuration](../configuration.md) |
| Changing state on a running node | [Runtime Control](../nvml-mock-ctl.md) |
