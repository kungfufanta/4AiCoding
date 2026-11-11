# Node resources

The console's **Nodes** page shows what every fleet member's machine really
has and uses (CPU, memory, disk, network), and lets an admin propose a new
advertised capacity as a pull request. Nothing on this page holds cluster or
hypervisor credentials.

## Host telemetry

Every sandboxd process that is a node samples the machine it runs on and
reports it:

| Node kind | How it reports | Disk reported by default |
|---|---|---|
| control plane (`local`) | in-process, every 10 s | the data dir (`SANDBOXD_DATA_DIR`) |
| `sandboxd worker` | with each heartbeat (`POST /internal/nodes/heartbeat`, field `telemetry`) | `/` |
| executor (`SANDBOXD_CONTROL_PLANE_URL` set) | with each heartbeat | the data dir |

The sample comes from procfs and statfs inside the worker image itself
(`internal/hosttelemetry`), so it works the same on a plain VPS, under Docker
or in a k3s pod. No Kubernetes Metrics API, Prometheus or node exporter is
needed.

`GET /v1/nodes` carries each node's latest sample (`telemetry`), and
`GET /v1/nodes/{id}/metrics` adds one hour of history (kept in memory, one
point per 5 s at most; it starts over when the control plane restarts).

### What a container sees

- **CPU and memory** (`/proc/stat`, `/proc/meminfo`, `/proc/loadavg`) are not
  namespaced, so a container or pod reports the **host's** numbers (unless
  lxcfs is installed).
- **Disk** is `statfs` of `SANDBOXD_TELEMETRY_DISK_PATH`. Inside a container
  `/` is the overlay on the host's container storage. To report another
  filesystem, mount it and point the variable at it.
- **Network** comes from `/proc/net/dev`, which is the reader's own network
  namespace. Loopback, bridges, veth pairs, overlays and VPN tunnels are not
  counted, so traffic is not counted twice. A container on a bridge network
  reports its own traffic (the console marks this as "container namespace").
  For the host's traffic either run with `--network host`, or mount the
  host's `/proc` read-only and set `SANDBOXD_HOST_PROC`:

```sh
docker run -d --name sandboxd-worker --restart unless-stopped \
  -v /proc:/host/proc:ro -e SANDBOXD_HOST_PROC=/host/proc \
  -v /:/host/root:ro -e SANDBOXD_TELEMETRY_DISK_PATH=/host/root \
  -e SANDBOXD_CONTROL_PLANE_URL=https://cp.example:9000 \
  -e SANDBOXD_WORKER_TOKEN=... -e SANDBOXD_NODE_ID=vps-1 \
  ghcr.io/kungfufanta/4aicoding-worker:main worker
```

With `SANDBOXD_HOST_PROC` set, the network counters are read from
`<proc>/1/net/dev` (the host init's namespace) and the hostname from
`<proc>/sys/kernel/hostname`.

On a VPS without Docker, `sandboxd worker` reads the host directly.

In the Helm chart, `telemetry.enabled` (default `true`) and
`telemetry.diskPath` control it. The default worker pod stays non-root and
read-only with no host mounts, so it reports the node's CPU, memory and
container storage, and its own pod traffic.

`SANDBOXD_TELEMETRY=off` disables telemetry on any node. A worker that talks
to an older control plane (one that rejects the `telemetry` field) logs once
and keeps heartbeating without it.

## Capacity change requests (GitOps)

The capacity a node advertises is only what the scheduler is told; it cannot
create CPU or RAM. To really grow a node:

1. Grow the machine (Proxmox VM vCPU / RAM / disk) or add a worker.
2. Wait for the node's telemetry to show the new size.
3. In **Nodes → node → Change capacity**, enter the new numbers, **Preview**,
   then **Open pull request**.
4. Review and merge the PR. The `deploy-k3s-1` workflow runs `helm upgrade`
   with the edited values file. Reverting the PR rolls it back.

The control plane checks each request before it touches the repo:

- `cpus` > 0 with at most 2 decimals, `memory_mb` ≥ 256, `disk_mb` ≥ 0;
- not more than the host reports (cores, total memory, disk size) in its
  latest telemetry;
- not more than `SANDBOXD_GITOPS_MAX_CPUS` / `_MEMORY_MB` / `_DISK_MB`
  (defaults 256 / 1 TiB / 64 TiB).

The PR edits only the `capacity:` block of the values file (comments and
layout kept; the edit is re-parsed and verified), lives on a branch
`capacity/<node>-<unix time>`, and records who asked, why, and the host's
numbers at request time. The `validate-deploy-values` workflow lints and
renders the chart with the edited file on the PR.

### Configuration (control plane)

| Variable | Meaning |
|---|---|
| `SANDBOXD_GITOPS_NODE_FILES` | `node=path` pairs, comma separated, e.g. `k3s-1=deploy/k3s-1-values.yaml`. Empty = feature off. |
| `SANDBOXD_GITOPS_REPO` | `owner/name` of the deploy repo. |
| `SANDBOXD_GITOPS_TOKEN` | Fine-grained GitHub token on that one repo with **Contents** and **Pull requests** read/write. Empty = preview only: the console shows the diff to commit by hand. |
| `SANDBOXD_GITOPS_BASE` | Branch PRs target (default `main`). |
| `SANDBOXD_GITOPS_MAX_*` | Optional ceilings, see above. |

Only admins can open a change request (`POST /v1/nodes/{id}/change-requests`);
every role can read the list (`GET`). Opening one is written to the audit log
as `node.change_request`.

A node outside Helm (a VPS or a plain Docker worker) takes its advertised
capacity from `SANDBOXD_CAPACITY_*` (or `--capacity-*`) when it starts;
change it there and restart the worker.

### Not covered

Changing the VM itself (Proxmox API) is out of scope: it would need a
hypervisor credential. If it is added later, it belongs in a GitHub Actions
job with a narrowly scoped Proxmox token and a required review, not in the
console.
