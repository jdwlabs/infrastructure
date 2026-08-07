# RAM Expansion Decision — Three Undersized Workers (Phase 4)

Status: DECISION — rebalance now, defer hardware purchase. No purchase made, no
`terraform apply` performed or proposed by this doc.

Scope: Phase 4 of the cluster memory-efficiency epic (JDWLABS-154), covering
JDWLABS-304's buy-vs-rebalance question for the three smallest workers in the
cluster. This is a research/decision doc — it recommends a direction and
estimates cost; it does not execute any change.

## The problem

Three workers are capped at 2.31 Gi allocatable / 3.80 Gi capacity, the
smallest nodes in the cluster:

| Node | Proxmox host | req:alloc (2026-08-05) |
|---|---|---|
| `talos-k3y-y3e` | pve2 | **86.3%** (tightest) |
| `talos-g1i-e3h` | pve4 | 69.8% |
| `talos-2qd-v0u` | pve3 | 65.8% |

(Source: JDWLABS-154 Phase 1 comment, 2026-08-05 — live `kubectl top`/`get
nodes` capture against the 8-node core cluster.) By contrast `talos-4h8-zy6`
(pve1) sits at 14.09 Gi allocatable / 44.9% requested, and `talos-lx0-6a4`
(pve5) at 61.28 Gi allocatable / 20.3% requested — the cluster's largest
reserve by a wide margin.

The three tight nodes aren't tight because of waste — Phase 1-3 of this epic
already right-sized the chart-level offenders. They're tight because of the
**physical host underneath them**: per `scenarios/cp-memory-resize.md`
(verified live 2026-07-03 via Proxmox API), pve2/pve3/pve4 each report only
**13G total RAM**, each already running one control-plane VM alongside the
worker, with **1.4–1.7G free** at the hypervisor level at the time of that
capture. `cp-memory-resize.md`'s own Option A (CP 4G→6G / worker 6G→4G on
these same three hosts) is a straight *reallocation* within that 13G ceiling,
not an increase — it takes memory the worker would otherwise have and gives
it to the CP. Whether that plan has actually been applied is itself worth
re-verifying live (its header still reads "Status: PLANNED" at the time of
writing); either way, there is no headroom left on these three hosts to grow
the worker's allocatable share without either adding physical RAM or moving
work off the node entirely. That is the buy-vs-rebalance choice this doc
addresses.

## Option 1 — buy DIMMs

### What generation? (inferred, not confirmed)

This repository does not document Proxmox host models, CPU generation,
motherboard, or DIMM type anywhere. A full-repo search (`docs/`, `scenarios/`,
`terraform/`) for DDR4/DDR5, NUC/Optiplex/ThinkCentre/EliteDesk, CPU
model/Xeon/Ryzen, motherboard, socket, or DIMM-slot references returned zero
hits. This is deliberate, not an oversight — `docs/cluster-overview.md`
states outright that "detailed inventory, capacity budgets, and network
specifics are maintained privately."

The only basis available is indirect: the odd, non-round physical RAM totals
recorded for these hosts (13G × 3, 28G on pve1, 123G on pve5) are consistent
with small-form-factor consumer/business desktop hardware — the class of
machine (Optiplex Micro / EliteDesk / ThinkCentre Tiny and similar) most
commonly used to build a homelab node fleet this size, and which skews toward
DDR4-era generations. That is a plausible inference, **not a confirmed fact**,
and the cost estimate below is bracketed accordingly (DDR4 as the primary
case, DDR5 as a hedge). Confirm before buying anything — see "Unverified"
below.

### Current market conditions (researched 2026-08-07)

DDR4 and DDR5 pricing are both running well above their pre-2025 baseline due
to an ongoing, AI-driven DRAM shortage: DRAM manufacturers (Samsung, SK
Hynix, Micron) have reallocated the large majority of wafer capacity toward
HBM for AI accelerators — HBM consumes roughly 3x the wafer capacity per bit
of standard DRAM, and the shift has been described as a structural capacity
decision, not a temporary bottleneck. DDR5 contract prices rose 90–95% in Q1
2026 alone (the largest single-quarter increase on record), and multiple
industry trackers put current retail levels at:

- **DDR4, 16GB module**: ~$50–80 (up sharply from pre-shortage pricing)
- **DDR4, 32GB kit (2×16GB)**: ~$150–200 new (was $50–90 as recently as
  October 2025); reported **used/second-hand kits that sold for ~$50 in
  2025 now go for ~$125+ in 2026** — the shortage has erased most of the
  usual new-vs-used discount, undermining the ticket's premise that
  second-hand sourcing is a materially cheaper path right now
- **DDR5, 32GB kit**: ~$400–550 (some listings to $650); ~$12–14/GB retail

Analysts do not expect relief soon — projections in the sources below range
from "into 2028" to one manufacturer (SK Hynix) warning the shortage could
persist past 2030.

### Cost estimate for these 3 hosts

Assuming a modest, meaningful bump (e.g., +16Gi per host) and bracketing for
both slot-availability and generation unknowns:

| Scenario | Per-host cost | 3-host total |
|---|---|---|
| DDR4, one free slot exists (single 16GB module) | ~$50–80 | **~$150–240** |
| DDR4, slots full / matched-pair required (32GB kit swap) | ~$150–200 | **~$450–600** |
| DDR4, second-hand (no longer a meaningful discount in this market) | ~$125+/kit | **~$375+** |
| DDR5, if generation assumption is wrong | ~$400–550/kit | **~$1,200–1,650** |

All rows assume DIMM slots are physically available and unoccupied/expandable
— the item this session could not verify (see below).

## Option 2 — rebalance onto pve5

`talos-lx0-6a4` (pve5) has 61.28 Gi allocatable at 20.3% requested — roughly
**48.8 Gi headroom**, the largest reserve in the cluster by a wide margin, and
already the designated landing zone for the platform's heaviest services:
Grafana, Loki, and kube-prometheus-stack all carry a *soft*
(`preferredDuringSchedulingIgnoredDuringExecution`) node affinity toward
`workload.jdwlabs.io/monitoring` — a label this repo's platform-side evidence
shows is carried only by the pve5 worker (`tenants/platform/services/{grafana,
loki,kube-prometheus-stack}/values.yaml`).

### Constraints on moving more workload there

1. **CNPG PostgreSQL is hard-anti-affine across hosts.**
   `tenants/platform/services/postgresql-cluster-{non,prd}/values.yaml` sets
   `podAntiAffinityType: required` with `topologyKey: kubernetes.io/hostname`
   for both clusters' 3 replicas each — the comment in that file notes this
   is required specifically *because* `topology.kubernetes.io/zone` is unset
   on every node in this homelab, making zone-level anti-affinity a no-op.
   Consequence: CNPG structurally requires **at least 3 distinct schedulable
   worker nodes**. Draining all CNPG pods off the three small workers onto
   pve5 alone is not just undesirable, it would leave replicas Pending —
   the scheduler has nowhere else to satisfy the `required` rule.
2. **Longhorn is node-anchored, not just pod-anchored.** Every worker,
   including the three small ones, carries `workload.jdwlabs.io/worker: "true"`
   (`tenants/platform/services/longhorn/values.yaml`), and Longhorn's own
   DaemonSets run on all of them by design. More importantly, Longhorn
   replica *data* lives on whichever node holds it; moving a Longhorn-backed
   workload's pod doesn't relocate its volume — that needs a separate
   eviction/rebuild cycle, not a scheduling change.
3. **Physical host-spread protection doesn't exist yet.** As above,
   `topology.kubernetes.io/zone` is unset cluster-wide, so nothing currently
   stops 3-replica Longhorn/CNPG spread from landing multiple replicas on
   VMs that share one physical Proxmox host. `scenarios/pve5-worker-rebalance.md`
   names this exact gap (its gate 6) as a hard precondition before retiring
   any more of the small workers, and it was never implemented.
4. **The broader "move to pve5, retire the small workers" strategy was
   already evaluated and explicitly deferred.** `scenarios/pve5-worker-rebalance.md`
   is marked **Status: DEFERRED (2026-07-21)** — "the worker-capacity problem
   this plan targeted was resolved by right-sizing... kept as reference for
   a future genuine worker-capacity need." Nothing found in this session
   overturns that call: the CNPG hard-anti-affinity and missing zone labels
   that made full retirement risky then are unchanged today. This doc does
   **not** recommend reopening that runbook.

### What *is* available: partial, soft rebalance

JDWLABS-304 doesn't require evacuating the three workers — only enough
placement relief to pull them out of hotspot territory (the epic's own Phase
1 target: each small worker ≤65% req:alloc, ≤70% use:alloc). The pattern
already proven for the monitoring stack — a soft, preferred `nodeAffinity`
toward pve5 — is directly reusable for any workload on the small workers that
is **not** subject to the CNPG hard-anti-affinity rule and is **not**
Longhorn-volume-anchored (e.g., stateless platform components, gateway/ingress
replicas without anti-affinity, ArgoCD components). It costs nothing, needs
no `terraform apply`, no node retirement, and is trivially reversible. It is
bounded by the same two constraints above — it cannot touch CNPG or move
Longhorn-backed data — but is otherwise unclaimed headroom.

## Recommendation

**Rebalance now (placement), defer hardware purchase — explicitly time-boxed
to current market conditions, not a permanent verdict.**

1. Do not buy DIMMs at this time. Current DDR4/DDR5 pricing runs at roughly
   2–5x the pre-2025-shortage baseline, with no relief expected before 2028
   at the earliest per the sources below. A ticket scoped as "cost cheap RAM
   expansion" cannot be satisfied by buying into the worst pricing point in a
   multi-year trend.
2. Extend the existing soft-nodeAffinity pattern (Grafana/Loki/
   kube-prometheus-stack → pve5) to the movable, non-CNPG, non-Longhorn-
   anchored workloads currently scheduled on `talos-k3y-y3e`,
   `talos-g1i-e3h`, and `talos-2qd-v0u`. This is a `platform` repo change
   (Helm values), not an `infrastructure`/Terraform change — out of scope
   for this doc's PR. File it as a follow-up ticket against `platform`.
3. Re-open the DIMM-purchase question opportunistically: when DRAM pricing
   normalizes, or if placement relief alone doesn't bring the three nodes
   under the Phase 1 target thresholds.
4. Before any future purchase decision, verify DIMM slot availability and
   populated configuration live — this research session had no cluster or
   physical access to do so. Two live-verifiable paths for whoever picks this
   up: (a) `talosctl -n <worker-ip> get memorymodules` (Talos exposes SMBIOS-
   derived hardware resources including memory modules; confirm the exact
   resource name against the cluster's installed Talos version first), or
   (b) SSH to the underlying Proxmox host (pve2/pve3/pve4 — an already-
   established access pattern per `docs/host-addressing.md`) and run
   `dmidecode -t memory` for authoritative slot count, population, and max
   capacity. Confirm DDR4 vs DDR5 and SODIMM vs UDIMM the same way rather
   than trusting the inference in this doc.

## Unverified — flagged explicitly, not guessed around

- **DIMM slot availability/population on pve2, pve3, pve4.** Needs live
  `talosctl`/Proxmox hardware inventory (SSH + `dmidecode`), unavailable to
  this offline research session. This is the load-bearing unknown for
  Option 1 — everything in the cost table above is conditional on it.
- **RAM generation (DDR4 vs DDR5) and form factor (SODIMM vs UDIMM).** No
  host model, CPU, or motherboard identifier exists anywhere in this repo;
  `docs/cluster-overview.md` states hardware inventory is deliberately kept
  private. The DDR4-primary assumption here rests only on the physical RAM
  totals in `scenarios/cp-memory-resize.md` being consistent with
  small-form-factor consumer/business desktop hardware typical of homelab
  fleets — a plausible inference, not a confirmed fact.
- **Whether `scenarios/cp-memory-resize.md`'s Option A has actually been
  applied.** Its own header still reads "Status: PLANNED." If it has been
  applied, it already reassigned worker memory to the CP on these same three
  hosts, tightening the exact number this ticket is about — re-verify the
  live CP/worker memory split before scoping any follow-up work.

## Sources — RAM pricing (retrieved 2026-08-07)

- [RAM price tracking 2026 — lowest price on DDR5 and DDR4 memory of all capacities (Tom's Hardware)](https://www.tomshardware.com/pc-components/ram/ram-price-index-2026-lowest-price-on-ddr5-and-ddr4-memory-of-all-capacities)
- [SODIMM RAM Prices 2026: Buy Now or Wait? DDR4 Price Trends (Honeybee-Technologies)](https://honeybee-technologies.com/blogs/news/sodimm-ram-prices-2026-buy-now-wait)
- [16GB RAM Prices 2026 | DDR4 & DDR5 16GB Module Price Tracker (rampricesusa.com)](https://rampricesusa.com/16gb-ram-prices)
- [DDR5 RAM Prices July 2026: 32GB, 64GB & Per-GB Kit Costs (techfuelhq.com)](https://techfuelhq.com/articles/ddr5-ram-buying-guide-2025/)
- [2026 could well be the year of the $500 32GB DDR5 memory module (TechRadar Pro)](https://www.techradar.com/pro/2026-could-well-be-the-usd500-32gb-ddr5-memory-module-experts-predict-ddr-will-go-up-by-60-percent-in-q1-2026-alone)
- [RAM Shortage 2026 Explained: Why AI Is Causing a DDR5 Crisis & When It Ends (Wccftech)](https://wccftech.com/roundup/memory-crisis/)
- [RAM Prices Could Double by End of 2026 as AI Consumes DRAM Faster Than It Can Be Made (abit.ee)](https://abit.ee/en/ddr-ram/ram-ddr5-dram-hbm-prices-ai-shortage-2026-en)
- [2026 Memory Chip Shortage: SK Hynix Warns It May Last Past 2030 (tech-insider.org)](https://tech-insider.org/memory-chip-shortage-2026-ai-consumer-electronics/)
- [The 2026 Memory Chip Shortage: Why Server RAM Prices Have Doubled and When It Ends (datacenterdisk.com)](https://datacenterdisk.com/news/memory-chip-shortage-2026-server-ram-prices)

## Related

- Epic: JDWLABS-154 (cluster memory-efficiency), Phase 1 baseline comment
  2026-08-05 — source of the per-node req:alloc figures above.
- `scenarios/cp-memory-resize.md` — CP/worker memory split on pve2/pve3/pve4;
  source of the 13G-per-host physical RAM figures.
- `scenarios/pve5-worker-rebalance.md` — the prior, broader "retire small
  workers onto pve5" evaluation; DEFERRED, still the standing decision for
  full evacuation.
