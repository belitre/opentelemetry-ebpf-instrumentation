# Fixes carried in this branch

This branch is OBI v3.38.0 (as vendored by Beyla v3.38.0) plus the fixes below, built into a custom Beyla image until
upstream releases them. Each entry has what is needed to open the upstream issue or PR. Once upstream has a fix, the
branch is dropped, not maintained.

Deployment where these were found: Beyla in network-only mode (`network`, `network_flow_packets`, `network_inter_zone`
features, `attributes.kubernetes.enable: true`, no k8s-cache), Helm chart 1.16.11 as a DaemonSet on EKS 1.35 with the
AWS VPC CNI, Amazon Linux 2023 nodes (kernel 6.12, x86_64 and aarch64), Karpenter creating and removing nodes all day.
Flow bytes were checked against the AWS Cost and Usage Report per instance and hour.

## 1. Flow bytes lost or counted twice on busy flows

- **Status:** fixed in this branch (`fix(netolly): stop losing and double counting flow bytes on busy flows`). No upstream
  issue or PR yet.
- **What happens:** the flow programs increment a flow's per-CPU value in place and then write it back with
  `bpf_map_update_elem(BPF_ANY)`. If user space deletes the entry between the lookup and the write-back, the write-back
  re-inserts it with bytes that were already read, so they are counted twice. New flows are inserted with `BPF_ANY`
  too, replacing an entry another CPU has just created and dropping what it counted. The readers also discard per-CPU
  values whose flow started before the last read, which only drops bytes that were not read yet (since Linux 5.10 an
  insert zero-fills the other CPUs' values).
- **Fix:** update existing entries in place only; insert new ones with `BPF_NOEXIST` and add the packet to the entry
  another CPU created when the insert races; stop filtering per-CPU values by start time.
- **Evidence:** on busy nodes, flow bytes did not add up to the billed bytes (both lost and double-counted bytes); with the
  fix, per-instance cross-AZ totals match the bill to within about 1%.

## 2. Kubernetes metadata lost for a Pod that reuses the IP of a finished Pod

- **Status:** fixed in this branch (`fix(kube): keep a Pod's metadata when it reuses the IP of a finished Pod`).
  Upstream issue: open-telemetry/opentelemetry-ebpf-instrumentation#3726 (open). The PR is not opened upstream yet; a
  review copy is belitre/opentelemetry-ebpf-instrumentation#1.
- **What happens:** a finished Pod keeps its IP in `Status.PodIPs`; when the CNI gives the IP to a new Pod and the finished
  Pod is then deleted or updated, the new Pod's entry is removed from the IP index and its flows lose their metadata until
  the agent restarts.
- **Fix:** `PodInfo` carries whether the Pod has finished, a finished Pod never takes an IP from a running one, and
  deletes only remove IP entries that still belong to the deleted object.

## 3. `metadata_retry.timeout` does not bound a hanging attempt (agent blind for 30 s on new nodes)

- **Status:** fixed in this branch (`fix(metadata): the retry timeout also bounds a hanging attempt`). No upstream issue or
  PR yet.
- **What happens:** the agent only starts collecting after the node metadata fetchers finish. `attributes.metadata_retry.timeout`
  (`OTEL_EBPF_METADATA_RETRY_TIMEOUT`, documented as the maximum total time for all retry attempts) is only checked
  between attempts, while the Kubernetes node fetcher gives each attempt its own hardcoded 30 s
  (`kubeTimeout` in `pkg/metadata/meta_node_kube.go`). On a node that has just joined, the agent can start before
  kube-proxy has programmed the `kubernetes` Service IP; its first API call (`Pods().List` in `fetchNodeName`) then
  hangs instead of failing, and the agent stays blind for 30 s whatever the retry timeout is. Anything a new Pod sends in
  that time is not counted.
- **Evidence:** with `metadata_retry.timeout: 2s`, 42 of 63 agent starts on new nodes in a few hours still logged
  `configuration loaded` and `starting Beyla in Network metrics mode` 30 s apart, with nothing in between; restarts on
  existing nodes took under 1 s. A batch job that starts on a fresh node with ~30 MB/s of traffic lost 10% of its bytes
  on those starts and ~0.4% on the others.
- **Repro:** unit test `TestFetchEntries_RetryTimeoutBoundsHangingAttempt` in `pkg/metadata/meta_node_test.go` (a fetcher
  that blocks until its context is done never returns on main).
- **Fix:** `backoffFetch` runs the attempts under a context with the retry timeout, so an attempt that hangs is cancelled
  when the retry time is up.

# Observed, not fixed

## A. A sending agent briefly reports a flow before it has identified its own Pod

- When a Pod opens a connection to a Pod on another node, the sending node's agent sometimes exports a series for that
  flow with the source unidentified (no `k8s_src_*`, the source named after its subnet) holding only a few hundred bytes,
  while the receiving agent reports the same flow, under the same labels, with the full byte count. The series stays at
  that value until it expires.
- Impact: anything that deduplicates the two agents' reports by averaging them halves the receiver's bytes while that
  series is new. Worked around outside OBI by preferring the receiving agent's series.
- To check before an issue: whether it is the first packets of a connection seen before the Pod's IP is in the local
  store, and whether it happens with the k8s-cache too.

## B. Inbound bytes 1–3% below the billed bytes

- After the fixes above, the bytes the receiving agent counts are 1–3% below what AWS bills for the same instance and hour
  (inbound only; outbound matches): ~2.3% on every node for traffic from RDS in a peered VPC, ~1–2% on cross-AZ traffic.
- Suspected: GRO merges TCP segments before the capture point, so the merged segments' IP/TCP headers are not counted,
  while the bill counts every segment. Unproven; to check with a controlled transfer at MSS 1448 and at MSS 8949 and
  compare the counted bytes with the segments sent. If confirmed, the capture could add the headers of
  `skb->gso_segs - 1` segments.
