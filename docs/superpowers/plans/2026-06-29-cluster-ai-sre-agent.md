# Cluster AI-SRE Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a provider-agnostic, always-on AI-SRE agent that monitors the Talos cluster, performs root-cause analysis on incidents, and proposes human-gated fixes.

**Architecture:** Adopt the HolmesGPT harness (in-cluster operator) for the agent loop + runbooks + toolsets; route all LLM calls through a self-hosted LiteLLM gateway so the backend (Claude now; Kimi/Codex/Ollama/self-hosted vLLM later) is a config swap; output to Jira + GitHub PR + Discord; trigger from Alertmanager + an hourly drift scan. Build is phased — Claude-only pipeline first (Phase 0–3), local RTX 5090 tier last (Phase 4).

**Tech Stack:** Kubernetes (Talos), ArgoCD GitOps via the platform tenant envelope, Helm, External Secrets Operator + Vault, CloudNativePG, kube-prometheus-stack (Alertmanager), Terraform (bpg/proxmox) for the GPU VM, vLLM.

## Global Constraints

- **GitOps only.** No `kubectl apply` / `helm install` by hand. Changes land by editing `tenants/platform/tenant.yaml` + per-service files, committed; ArgoCD reconciles. Local verification uses `helm template` and `kubectl --dry-run=client`, never live apply.
- **Two repos, two worktrees.** Platform manifests → `platform` repo (create worktree `feat/cluster-ai-sre-agent` there). GPU VM terraform + these docs → `infrastructure` repo (this worktree). Tasks state which repo.
- **Secrets via Vault + ESO only.** Reference `ClusterSecretStore` named `vault`. Never commit plaintext secrets. Vault seeding is a manual human step (explicitly marked) — agents do not write Vault.
- **Terraform is plan-and-stop.** `terraform apply` / `destroy` are NEVER run autonomously (infra repo AI contract). Produce a plan, await human apply.
- **Read-only toolsets in v1.** Custom toolsets expose only query verbs (`get`, `describe`, `member list`, promQL). No `delete`, `edit`, `reset`. Remediation leaves only as a human-merged PR.
- **Commit per task; never push.** Human pushes and opens PRs.
- **Namespace:** all agent components live in namespace `ai-sre`.
- **syncWave:** new components use wave `5` (after ESO/Vault wave 2, CNPG operator wave 3, DB wave 4), except the CNPG `litellm-db` Cluster which is wave `4`.
- **Pinned versions (verbatim, override only with cause):** litellm-helm `oci://docker.litellm.ai/berriai/litellm-helm`; holmes chart `robusta/holmes`; cnpg cluster chart `cluster` from `https://cloudnative-pg.github.io/charts` rev `0.4.0` (matches existing `postgresql-cluster-prd`).

---

## File Structure

**platform repo** (`feat/cluster-ai-sre-agent` worktree):
- `tenants/platform/tenant.yaml` — MODIFY: add `ai-sre` namespace + four `applications` entries (litellm-db, redis via rawManifests, litellm, holmes).
- `tenants/platform/services/litellm-db/values.yaml` — CREATE: CNPG Cluster values for the gateway's Postgres.
- `tenants/platform/services/litellm-redis/postInstall/redis.yaml` — CREATE: minimal Redis Deployment+Service (rawManifests).
- `tenants/platform/services/litellm/values.yaml` — CREATE: LiteLLM proxy config (model_list, db, redis, master key from env).
- `tenants/platform/services/litellm/postInstall/externalsecret.yaml` — CREATE: ESO pulls ANTHROPIC_API_KEY + master key from Vault.
- `tenants/platform/services/holmes/values.yaml` — CREATE: HolmesGPT model→LiteLLM, toolsets, custom runbooks/skills, RBAC.
- `tenants/platform/services/holmes/postInstall/externalsecret.yaml` — CREATE: ESO pulls Jira/GitHub/Discord creds + the LiteLLM key.
- `tenants/platform/services/holmes/runbooks/` — CREATE: runbook markdown mounted as customSkills.
- `tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml` — MODIFY: add a `holmes` webhook receiver + route (continue, so Discord still fires).

**infrastructure repo** (this worktree):
- `terraform/gpu-node.tf` — CREATE (Phase 4): Proxmox Ubuntu VM on pve5 with RTX 5090 PCIe passthrough.
- `terraform/variables.tf` — MODIFY (Phase 4): add GPU VM variables.
- `scenarios/ai-sre-agent-runbook.md` — CREATE: operate/troubleshoot the agent.

---

## Phase 0 — LiteLLM gateway foundation (platform repo)

### Task 0.1: Add `ai-sre` namespace to the tenant envelope

**Files:**
- Modify: `tenants/platform/tenant.yaml` (the `namespaces:` list)

**Interfaces:**
- Produces: namespace `ai-sre` (governance-managed; charts must set `namespace.create: false`).

- [ ] **Step 1: Add the namespace entry**

Append to the `namespaces:` list in `tenants/platform/tenant.yaml`:

```yaml
  - name: ai-sre
    quotaTier: platform
    networkPolicy: platform
```

- [ ] **Step 2: Verify YAML parses**

Run: `python -c "import yaml,sys; yaml.safe_load(open('tenants/platform/tenant.yaml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Commit**

```bash
git add tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): add ai-sre namespace to platform tenant envelope"
```

### Task 0.2: CloudNativePG database for LiteLLM

**Files:**
- Create: `tenants/platform/services/litellm-db/values.yaml`
- Modify: `tenants/platform/tenant.yaml` (`applications:` list)

**Interfaces:**
- Produces: a CNPG `Cluster` named `litellm-db` in `ai-sre`; CNPG generates secret `litellm-db-app` (keys: `username`, `password`, `dbname`, `host`, `uri`) consumed by Task 0.5.

- [ ] **Step 1: Create the CNPG cluster values**

`tenants/platform/services/litellm-db/values.yaml`:

```yaml
# Single-instance Postgres for the LiteLLM gateway (keys, budgets, spend log).
# Mirrors the existing postgresql-cluster-prd pattern (cnpg 'cluster' chart 0.4.0).
namespace:
  create: false

type: postgresql
mode: standalone

cluster:
  instances: 1
  storage:
    size: 5Gi
    storageClass: longhorn
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      memory: 512Mi

# CNPG auto-creates an 'app' database + 'litellm-db-app' secret with the URI.
bootstrap:
  initdb:
    database: litellm
    owner: litellm
```

- [ ] **Step 2: Add the application entry**

Append to `applications:` in `tenants/platform/tenant.yaml`:

```yaml
  - name: litellm-db
    chart: cluster
    repo: https://cloudnative-pg.github.io/charts
    revision: 0.4.0
    namespace: ai-sre
    postInstall: false
    syncWave: 4
```

- [ ] **Step 3: Verify the chart templates with these values**

Run:
```bash
helm template litellm-db oci://cloudnative-pg.github.io/charts/cluster --version 0.4.0 \
  -f tenants/platform/services/litellm-db/values.yaml --namespace ai-sre > /tmp/litellm-db.yaml; \
grep -q "kind: Cluster" /tmp/litellm-db.yaml && echo "RENDERED-OK"
```
Expected: `RENDERED-OK` (if the repo must be added first: `helm repo add cnpg https://cloudnative-pg.github.io/charts && helm repo update`, then `helm template litellm-db cnpg/cluster --version 0.4.0 ...`).

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/litellm-db/values.yaml tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): add CNPG postgres for litellm gateway"
```

### Task 0.3: Redis for LiteLLM routing/cache

**Files:**
- Create: `tenants/platform/services/litellm-redis/postInstall/redis.yaml`
- Modify: `tenants/platform/tenant.yaml` (`applications:` list)

**Interfaces:**
- Produces: Service `litellm-redis.ai-sre.svc:6379` (no auth, ClusterIP, ephemeral) consumed by Task 0.5.

- [ ] **Step 1: Create the Redis manifests**

`tenants/platform/services/litellm-redis/postInstall/redis.yaml`:

```yaml
# Ephemeral Redis for LiteLLM cross-replica routing + response cache.
# No persistence: routing/cache state is reconstructable; loss is harmless.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: litellm-redis
  namespace: ai-sre
spec:
  replicas: 1
  selector:
    matchLabels: { app: litellm-redis }
  template:
    metadata:
      labels: { app: litellm-redis }
    spec:
      containers:
        - name: redis
          image: redis:7.4-alpine
          args: ["--save", "", "--appendonly", "no"]
          ports:
            - containerPort: 6379
          resources:
            requests: { cpu: 25m, memory: 64Mi }
            limits: { memory: 128Mi }
---
apiVersion: v1
kind: Service
metadata:
  name: litellm-redis
  namespace: ai-sre
spec:
  selector: { app: litellm-redis }
  ports:
    - port: 6379
      targetPort: 6379
```

- [ ] **Step 2: Add the application entry (rawManifests)**

Append to `applications:` in `tenants/platform/tenant.yaml`:

```yaml
  - name: litellm-redis
    chart: ""
    repo: ""
    revision: ""
    namespace: ai-sre
    rawManifests: true
    postInstall: false
    syncWave: 4
```

- [ ] **Step 3: Verify manifests are valid**

Run: `kubectl apply --dry-run=client -f tenants/platform/services/litellm-redis/postInstall/redis.yaml`
Expected: `deployment.apps/litellm-redis created (dry run)` and `service/litellm-redis created (dry run)`

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/litellm-redis tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): add ephemeral redis for litellm"
```

### Task 0.4: Vault seed (manual) + ExternalSecret for LiteLLM

**Files:**
- Create: `tenants/platform/services/litellm/postInstall/externalsecret.yaml`

**Interfaces:**
- Consumes: Vault KV at path `litellm` with properties `anthropic_api_key`, `master_key`.
- Produces: k8s Secret `litellm-secrets` in `ai-sre` (keys: `ANTHROPIC_API_KEY`, `LITELLM_MASTER_KEY`) consumed by Task 0.5.

- [ ] **Step 1: HUMAN — seed Vault**

This step is performed by a human, not the agent. Store the Anthropic key and a generated master key (must start with `sk-`):

```bash
vault kv put secret/litellm \
  anthropic_api_key="sk-ant-..." \
  master_key="sk-$(openssl rand -hex 24)"
```

Record the master key — Holmes needs it in Task 2.x.

- [ ] **Step 2: Create the ExternalSecret**

`tenants/platform/services/litellm/postInstall/externalsecret.yaml`:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: litellm-secrets
  namespace: ai-sre
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault
    kind: ClusterSecretStore
  target:
    name: litellm-secrets
    creationPolicy: Owner
  data:
    - secretKey: ANTHROPIC_API_KEY
      remoteRef:
        key: litellm
        property: anthropic_api_key
    - secretKey: LITELLM_MASTER_KEY
      remoteRef:
        key: litellm
        property: master_key
```

- [ ] **Step 3: Verify manifest validity**

Run: `kubectl apply --dry-run=client -f tenants/platform/services/litellm/postInstall/externalsecret.yaml`
Expected: `externalsecret.external-secrets.io/litellm-secrets created (dry run)`

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/litellm/postInstall/externalsecret.yaml
git commit -m "feat(ai-sre): externalsecret for litellm provider keys"
```

### Task 0.5: Deploy LiteLLM proxy

**Files:**
- Create: `tenants/platform/services/litellm/values.yaml`
- Modify: `tenants/platform/tenant.yaml` (`applications:` list)

**Interfaces:**
- Consumes: Secret `litellm-secrets` (Task 0.4), `litellm-db-app` (Task 0.2), `litellm-redis` Service (Task 0.3).
- Produces: Service `litellm.ai-sre.svc:4000` exposing an OpenAI-compatible API; model alias `claude-sonnet` routes to `anthropic/claude-sonnet-4-6`. Master key in `LITELLM_MASTER_KEY`.

- [ ] **Step 1: Create LiteLLM values**

`tenants/platform/services/litellm/values.yaml`:

```yaml
# Provider-agnostic LLM gateway. Backend swap = edit model_list, no client change.
namespace:
  create: false

masterkeySecretName: litellm-secrets
masterkeySecretKey: LITELLM_MASTER_KEY

environmentSecrets:
  - litellm-secrets        # ANTHROPIC_API_KEY

# DATABASE_URL from the CNPG-generated secret (key 'uri').
envVars:
  STORE_MODEL_IN_DB: "True"
extraEnvVars:
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef:
        name: litellm-db-app
        key: uri

proxy_config:
  model_list:
    # Frontier tier — hard root-cause reasoning.
    - model_name: claude-sonnet
      litellm_params:
        model: anthropic/claude-sonnet-4-6
        api_key: os.environ/ANTHROPIC_API_KEY
    # Phase 4 adds: local-qwen -> hosted_vllm/... api_base http://<pve5-ip>:8000
  router_settings:
    redis_host: litellm-redis.ai-sre.svc.cluster.local
    redis_port: 6379
  litellm_settings:
    drop_params: true
    # Fallbacks: if frontier unavailable, degrade (local tier added Phase 4).
    num_retries: 2

resources:
  requests: { cpu: 100m, memory: 256Mi }
  limits: { memory: 512Mi }
```

> Note: confirm exact key names against `oci://docker.litellm.ai/berriai/litellm-helm` `values.yaml` during Step 3; the chart accepts `proxy_config` (the proxy `config.yaml`), `environmentSecrets` (envFrom secret names), `masterkeySecretName/Key`, and `extraEnvVars`. Adjust casing to match the rendered output.

- [ ] **Step 2: Add the application entry**

Append to `applications:` in `tenants/platform/tenant.yaml`:

```yaml
  - name: litellm
    chart: litellm-helm
    repo: oci://docker.litellm.ai/berriai
    revision: 0.1.2
    namespace: ai-sre
    postInstall: true
    syncWave: 5
```

- [ ] **Step 3: Verify the chart renders**

Run:
```bash
helm template litellm oci://docker.litellm.ai/berriai/litellm-helm --version 0.1.2 \
  -f tenants/platform/services/litellm/values.yaml --namespace ai-sre > /tmp/litellm.yaml; \
grep -q "kind: Deployment" /tmp/litellm.yaml && echo "RENDERED-OK"
```
Expected: `RENDERED-OK`. Inspect `/tmp/litellm.yaml` to confirm `DATABASE_URL`, `LITELLM_MASTER_KEY`, and `ANTHROPIC_API_KEY` reach the container; fix value keys if not.

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/litellm/values.yaml tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): deploy litellm gateway (claude backend)"
```

### Task 0.6: PHASE 0 GATE — gateway answers through Claude

This gate runs after the human merges the PR and ArgoCD has synced `ai-sre`.

- [ ] **Step 1: Confirm pods healthy**

Run: `kubectl get pods -n ai-sre`
Expected: `litellm-*`, `litellm-db-1`, `litellm-redis-*` all `Running`; ESO secret present (`kubectl get secret litellm-secrets -n ai-sre`).

- [ ] **Step 2: Call the gateway**

Run:
```bash
kubectl port-forward -n ai-sre svc/litellm 4000:4000 &
MK=$(kubectl get secret litellm-secrets -n ai-sre -o jsonpath='{.data.LITELLM_MASTER_KEY}' | base64 -d)
curl -s http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer $MK" -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet","messages":[{"role":"user","content":"reply with the single word: pong"}]}'
```
Expected: JSON completion containing `pong`. **Gate passes when Claude responds through the gateway.**

---

## Phase 1 — HolmesGPT detect+explain, Claude-only, manual (platform repo)

### Task 1.1: Vault seed (manual) + ExternalSecret for Holmes LLM key

**Files:**
- Create: `tenants/platform/services/holmes/postInstall/externalsecret.yaml`

**Interfaces:**
- Consumes: Vault KV `holmes` property `litellm_key` (the LiteLLM master key from Task 0.4, or a scoped virtual key minted in LiteLLM).
- Produces: Secret `holmes-llm` in `ai-sre`, key `OPENAI_API_KEY` (Holmes uses OpenAI-format auth against LiteLLM).

- [ ] **Step 1: HUMAN — seed Vault**

```bash
vault kv put secret/holmes litellm_key="<the LITELLM_MASTER_KEY or a minted virtual key>"
```

- [ ] **Step 2: Create the ExternalSecret**

`tenants/platform/services/holmes/postInstall/externalsecret.yaml`:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: holmes-llm
  namespace: ai-sre
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault
    kind: ClusterSecretStore
  target:
    name: holmes-llm
    creationPolicy: Owner
  data:
    - secretKey: OPENAI_API_KEY
      remoteRef:
        key: holmes
        property: litellm_key
```

- [ ] **Step 3: Verify + commit**

Run: `kubectl apply --dry-run=client -f tenants/platform/services/holmes/postInstall/externalsecret.yaml`
Expected: `... created (dry run)`

```bash
git add tenants/platform/services/holmes/postInstall/externalsecret.yaml
git commit -m "feat(ai-sre): externalsecret for holmes->litellm key"
```

### Task 1.2: Deploy HolmesGPT pointed at the gateway, built-in toolsets

**Files:**
- Create: `tenants/platform/services/holmes/values.yaml`
- Modify: `tenants/platform/tenant.yaml` (`applications:` list)

**Interfaces:**
- Consumes: Secret `holmes-llm` (Task 1.1), Service `litellm.ai-sre.svc:4000` (Task 0.5).
- Produces: Holmes Deployment + Service `holmes.ai-sre.svc` with `/api/investigate` (HTTP) for later Alertmanager wiring; model `claude-sonnet` via LiteLLM.

- [ ] **Step 1: Create Holmes values**

`tenants/platform/services/holmes/values.yaml`:

```yaml
# Holmes talks to LiteLLM as an OpenAI-compatible endpoint; backend stays swappable.
additionalEnvVars:
  - name: OPENAI_API_KEY
    valueFrom:
      secretKeyRef:
        name: holmes-llm
        key: OPENAI_API_KEY
  - name: OPENAI_API_BASE
    value: "http://litellm.ai-sre.svc.cluster.local:4000/v1"

modelList:
  claude-sonnet:
    model: openai/claude-sonnet      # 'openai/' = use OpenAI-compatible client
    api_base: http://litellm.ai-sre.svc.cluster.local:4000/v1
    api_key: "{{ env.OPENAI_API_KEY }}"
    temperature: 0

# v1: read-only investigation toolsets only.
toolsets:
  kubernetes/core:
    enabled: true
  kubernetes/logs:
    enabled: true
  prometheus/metrics:
    enabled: true
    config:
      prometheus_url: http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090
  internet:
    enabled: false

k8sRBAC: true
```

- [ ] **Step 2: Add the application entry**

Append to `applications:` in `tenants/platform/tenant.yaml`:

```yaml
  - name: holmes
    chart: holmes
    repo: https://robusta-charts.storage.googleapis.com
    revision: 0.0.0   # pin to latest holmes chart at implementation time
    namespace: ai-sre
    postInstall: true
    syncWave: 5
```

> Before committing, resolve the real chart version: `helm repo add robusta https://robusta-charts.storage.googleapis.com && helm repo update && helm search repo robusta/holmes --versions | head`. Replace `0.0.0` with the chosen pinned version.

- [ ] **Step 3: Verify the chart renders**

Run:
```bash
helm template holmes robusta/holmes --version <pinned> \
  -f tenants/platform/services/holmes/values.yaml --namespace ai-sre > /tmp/holmes.yaml; \
grep -q "OPENAI_API_BASE" /tmp/holmes.yaml && echo "RENDERED-OK"
```
Expected: `RENDERED-OK`.

- [ ] **Step 4: Commit**

```bash
git add tenants/platform/services/holmes/values.yaml tenants/platform/tenant.yaml
git commit -m "feat(ai-sre): deploy holmesgpt via litellm gateway"
```

### Task 1.3: PHASE 1 GATE — replay a past fault via `holmes ask`

Runs after merge + sync.

- [ ] **Step 1: Confirm Holmes healthy**

Run: `kubectl get pods -n ai-sre -l app.kubernetes.io/name=holmes`
Expected: `Running`.

- [ ] **Step 2: Ask Holmes about a real condition**

Run:
```bash
kubectl exec -n ai-sre deploy/holmes -- holmes ask \
  "Which pods in the cluster are currently OOMKilled or CrashLoopBackOff, and what is the most likely root cause?"
```
Expected: a coherent root-cause narrative referencing real pods, produced via Claude through LiteLLM. **Gate passes when the answer is cluster-accurate and routed through the gateway** (confirm with `kubectl logs -n ai-sre deploy/litellm | grep claude-sonnet`).

---

## Phase 2 — A-CHECKPOINT: event-driven + outputs, Claude-only (platform repo)

This is the shippable milestone: full pipeline on Claude, no local model yet.

### Task 2.1: Wire Alertmanager → Holmes webhook (keep Discord)

**Files:**
- Modify: `tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml`

**Interfaces:**
- Consumes: Holmes Service `holmes.ai-sre.svc` (Task 1.2).
- Produces: critical/warning alerts POST to Holmes; Discord still receives all (route uses `continue: true`).

- [ ] **Step 1: Add a `holmes` receiver and route**

In the `alertmanager.yaml` template within that ExternalSecret, add to `receivers:`:

```yaml
            - name: 'holmes'
              webhook_configs:
                - url: 'http://holmes.ai-sre.svc.cluster.local/api/alertmanager'
                  send_resolved: false
```

And add as the FIRST entry under `route.routes:` (so it sees alerts, then falls through to Discord):

```yaml
              - matchers:
                  - severity =~ "critical|warning"
                receiver: 'holmes'
                continue: true
```

- [ ] **Step 2: Verify the embedded YAML is valid**

Run:
```bash
python -c "import yaml; d=yaml.safe_load(open('tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml')); print('ok')"
```
Expected: `ok`. (The Alertmanager config itself is templated; also eyeball that `holmes` appears in both `receivers` and `routes`.)

- [ ] **Step 3: Commit**

```bash
git add tenants/platform/services/kube-prometheus-stack/postInstall/alertmanager-config-externalsecret.yaml
git commit -m "feat(ai-sre): route critical/warning alerts to holmes (discord retained)"
```

### Task 2.2: Vault seed (manual) + ExternalSecret for Jira/GitHub/Discord

**Files:**
- Modify: `tenants/platform/services/holmes/postInstall/externalsecret.yaml` (extend with integration creds)

**Interfaces:**
- Consumes: Vault KV `holmes` properties `jira_url`, `jira_email`, `jira_api_token`, `github_token`, `discord_webhook_url`.
- Produces: Secret `holmes-llm` gains keys `JIRA_URL`, `JIRA_USERNAME`, `JIRA_API_TOKEN`, `GITHUB_TOKEN`, `DISCORD_WEBHOOK_URL`.

- [ ] **Step 1: HUMAN — seed Vault**

```bash
vault kv patch secret/holmes \
  jira_url="https://<your>.atlassian.net" \
  jira_email="jdwillmsen@gmail.com" \
  jira_api_token="<token>" \
  github_token="<fine-grained PAT, PR scope on infrastructure/platform/deployments>" \
  discord_webhook_url="<holmes discord webhook>"
```

- [ ] **Step 2: Extend the ExternalSecret `data:` list**

Add to `data:` in `tenants/platform/services/holmes/postInstall/externalsecret.yaml`:

```yaml
    - secretKey: JIRA_URL
      remoteRef: { key: holmes, property: jira_url }
    - secretKey: JIRA_USERNAME
      remoteRef: { key: holmes, property: jira_email }
    - secretKey: JIRA_API_TOKEN
      remoteRef: { key: holmes, property: jira_api_token }
    - secretKey: GITHUB_TOKEN
      remoteRef: { key: holmes, property: github_token }
    - secretKey: DISCORD_WEBHOOK_URL
      remoteRef: { key: holmes, property: discord_webhook_url }
```

- [ ] **Step 3: Verify + commit**

Run: `kubectl apply --dry-run=client -f tenants/platform/services/holmes/postInstall/externalsecret.yaml`
Expected: `... created (dry run)`

```bash
git add tenants/platform/services/holmes/postInstall/externalsecret.yaml
git commit -m "feat(ai-sre): add jira/github/discord creds to holmes secret"
```

### Task 2.3: Configure Holmes outputs (Jira issue, Discord ping, GitHub PR)

**Files:**
- Modify: `tenants/platform/services/holmes/values.yaml`

**Interfaces:**
- Consumes: integration env from Secret `holmes-llm` (Task 2.2).
- Produces: on investigation, Holmes opens/updates a Jira issue, pings Discord, and (where the fix is GitOps-expressible) opens a GitHub PR linked to the issue.

- [ ] **Step 1: Add integration env + toolsets to Holmes values**

Add to `additionalEnvVars:` in `tenants/platform/services/holmes/values.yaml`:

```yaml
  - name: JIRA_URL
    valueFrom: { secretKeyRef: { name: holmes-llm, key: JIRA_URL } }
  - name: JIRA_USERNAME
    valueFrom: { secretKeyRef: { name: holmes-llm, key: JIRA_USERNAME } }
  - name: JIRA_API_TOKEN
    valueFrom: { secretKeyRef: { name: holmes-llm, key: JIRA_API_TOKEN } }
  - name: GITHUB_TOKEN
    valueFrom: { secretKeyRef: { name: holmes-llm, key: GITHUB_TOKEN } }
  - name: DISCORD_WEBHOOK_URL
    valueFrom: { secretKeyRef: { name: holmes-llm, key: DISCORD_WEBHOOK_URL } }
```

Enable the matching toolsets under `toolsets:`:

```yaml
  atlassian/jira:
    enabled: true
  github:
    enabled: true
  notification/discord:
    enabled: true
```

> Toolset key names vary by Holmes chart version. During Step 2, list available toolsets (`kubectl exec -n ai-sre deploy/holmes -- holmes toolset list`) and match exact names; adjust if different (e.g. `jira` vs `atlassian/jira`).

- [ ] **Step 2: Verify render + commit**

Run:
```bash
helm template holmes robusta/holmes --version <pinned> \
  -f tenants/platform/services/holmes/values.yaml --namespace ai-sre | grep -q "JIRA_URL" && echo "RENDERED-OK"
```
Expected: `RENDERED-OK`

```bash
git add tenants/platform/services/holmes/values.yaml
git commit -m "feat(ai-sre): enable holmes jira/github/discord outputs"
```

### Task 2.4: PHASE 2 GATE — synthetic alert produces Jira + Discord (+ PR)

Runs after merge + sync.

- [ ] **Step 1: Fire a synthetic alert**

Deploy a deliberately-crashing pod to trip `KubePodCrashLooping`:
```bash
kubectl run crashtest --image=busybox -n ai-sre --restart=Always -- /bin/sh -c "exit 1"
```
Wait for Alertmanager to fire (a few minutes).

- [ ] **Step 2: Confirm the pipeline end-to-end**

Expected, within ~10 min:
- A Jira issue created with root cause + suggested fix.
- A Discord message in the alerts channel.
- (If the synthetic fault were GitOps-expressible, a PR — not expected for a manual crashtest.)

Verify Holmes received the alert: `kubectl logs -n ai-sre deploy/holmes | grep -i alertmanager`.

- [ ] **Step 3: Clean up**

Run: `kubectl delete pod crashtest -n ai-sre`
Expected: pod deleted. **Gate passes when Jira + Discord both fire from a single alert.**

---

## Phase 3 — Custom toolsets + runbooks for infra faults (platform repo)

### Task 3.1: Add `talosctl` / `etcdctl` read-only toolsets

**Files:**
- Modify: `tenants/platform/services/holmes/values.yaml`

**Interfaces:**
- Produces: Holmes tools `talos_health`, `talos_etcd_members`, `talos_dmesg` (read-only) usable in investigations.

- [ ] **Step 1: HUMAN — provide talosconfig**

A human stores a **read-only** talosconfig in Vault and adds it to the Holmes secret (extend Task 2.2 ExternalSecret with `secretKey: talosconfig` from `holmes/talosconfig`), mounted at `/var/run/secrets/talos/config`. Talos API is read-only by capability here; the toolset must use only `get`/`health`/`list` verbs.

- [ ] **Step 2: Define custom YAML toolsets**

Add under `toolsets:` in `tenants/platform/services/holmes/values.yaml`:

```yaml
  talos/readonly:
    enabled: true
    tools:
      - name: talos_health
        description: "Cluster + node health via talosctl (read-only)."
        command: "talosctl --talosconfig /var/run/secrets/talos/config health --nodes {{ node }}"
      - name: talos_etcd_members
        description: "List etcd members + status (read-only). Use for quorum issues."
        command: "talosctl --talosconfig /var/run/secrets/talos/config etcd members --nodes {{ node }}"
      - name: talos_dmesg
        description: "Kernel ring buffer for a node (read-only). Use for VXLAN/NIC faults."
        command: "talosctl --talosconfig /var/run/secrets/talos/config dmesg --nodes {{ node }}"
```

Mount the secret as a file (add to `additional_env_froms`/volume per chart; verify exact key in Step 3).

- [ ] **Step 3: Verify render + commit**

Run: `helm template holmes robusta/holmes --version <pinned> -f tenants/platform/services/holmes/values.yaml | grep -q "talos_etcd_members" && echo OK`
Expected: `OK`

```bash
git add tenants/platform/services/holmes/values.yaml
git commit -m "feat(ai-sre): add read-only talosctl/etcdctl toolsets"
```

### Task 3.2: Load `.remember/` runbooks as custom skills

**Files:**
- Create: `tenants/platform/services/holmes/runbooks/vxlan-blackhole.md`
- Create: `tenants/platform/services/holmes/runbooks/etcd-ip-flip.md`
- Create: `tenants/platform/services/holmes/runbooks/hugepages-oom.md`
- Modify: `tenants/platform/services/holmes/values.yaml`

**Interfaces:**
- Produces: Holmes consults these runbooks before investigating matching alerts (`customSkills`/`customSkillPaths`).

- [ ] **Step 1: Write the VXLAN runbook**

`tenants/platform/services/holmes/runbooks/vxlan-blackhole.md`:

```markdown
# Runbook: VXLAN cross-node overlay blackhole

## Symptoms
Pod-to-pod traffic across nodes silently dropped (e.g. 4h8-zy6 -> 2qd-v0u).
DNS/service calls time out only for cross-node destinations; same-node OK.

## Diagnose
1. talos_dmesg on the affected nodes — look for flannel/VXLAN tx-checksum errors.
2. kubernetes/core — confirm the source/target pods are on different nodes.
3. prometheus/metrics — node network error counters.

## Root cause (historical)
Flannel VXLAN offload corruption. NOT a memory issue.

## Suggested fix (human-applied)
Reboot the affected node. Durable fix: vxlan-offload-fix service (already deployed).
```

- [ ] **Step 2: Write the etcd IP-flip runbook**

`tenants/platform/services/holmes/runbooks/etcd-ip-flip.md`:

```markdown
# Runbook: etcd quorum loss from control-plane IP flip

## Symptoms
Kube API unreachable; etcd peer mesh broken. A CP node's IP changed (DHCP),
e.g. talos-6iz-oey .240 -> .98.

## Diagnose
1. talos_etcd_members — compare member peer URLs vs current node IPs.
2. talos_health on each CP node.

## Root cause (historical)
CP node DHCP lease changed its IP; etcd peers still point at the old address.

## Suggested fix (human-applied)
Hard-reset affected CP nodes; restore quorum. Durable fix: static IPs / DHCP
reservations for control-plane nodes.
```

- [ ] **Step 3: Write the hugepages-OOM runbook**

`tenants/platform/services/holmes/runbooks/hugepages-oom.md`:

```markdown
# Runbook: BestEffort OOM from stranded hugepages

## Symptoms
BestEffort pods (ESO, cert-manager, etc.) OOMKilled despite apparent free memory.

## Diagnose
1. kubernetes/core — describe the node; check hugepages reserved vs used.
2. prometheus/metrics — node_memory_HugePages_* series.

## Root cause (historical)
Workers reserved ~2Gi unused hugepages; the OOM controller killed BestEffort
pods. VXLAN-memory correlation was a red herring.

## Suggested fix (GitOps PR)
Remove hugepages reservation in worker config; restart kubelet.
```

- [ ] **Step 4: Wire runbooks into Holmes**

Add to `tenants/platform/services/holmes/values.yaml`:

```yaml
customSkills:
  vxlan-blackhole.md: |-
    {{ .Files.Get "runbooks/vxlan-blackhole.md" }}
  etcd-ip-flip.md: |-
    {{ .Files.Get "runbooks/etcd-ip-flip.md" }}
  hugepages-oom.md: |-
    {{ .Files.Get "runbooks/hugepages-oom.md" }}
```

> If the chart cannot `.Files.Get` outside the chart dir, instead create a ConfigMap from the runbooks in `postInstall/runbooks-configmap.yaml` and set `customSkillPaths: ["/etc/holmes/runbooks"]` with a volume mount. Verify which the pinned chart supports in Step 5.

- [ ] **Step 5: Verify + commit**

Run: `helm template holmes robusta/holmes --version <pinned> -f tenants/platform/services/holmes/values.yaml | grep -q "vxlan-blackhole" && echo OK`
Expected: `OK`

```bash
git add tenants/platform/services/holmes/runbooks tenants/platform/services/holmes/values.yaml
git commit -m "feat(ai-sre): load .remember runbooks as holmes skills"
```

### Task 3.3: PHASE 3 GATE — replay infra faults against runbooks

- [ ] **Step 1: Replay the etcd scenario question**

Run:
```bash
kubectl exec -n ai-sre deploy/holmes -- holmes ask \
  "etcd reports a member with an unreachable peer URL. Walk the runbook and identify the root cause."
```
Expected: answer cites the etcd-ip-flip runbook, names the DHCP IP-flip root cause, recommends static IPs — and used `talos_etcd_members`.

- [ ] **Step 2: Replay the VXLAN scenario question**

Run:
```bash
kubectl exec -n ai-sre deploy/holmes -- holmes ask \
  "Cross-node pod traffic is silently dropping but same-node works. Diagnose."
```
Expected: cites the VXLAN runbook, identifies overlay blackhole, recommends node reboot. **Gate passes when both replays reach the documented root cause and cite the runbook.**

---

## Phase 4 — Local RTX 5090 tier (infrastructure + platform repos)

### Task 4.1: Provision the GPU inference VM on pve5 (infrastructure repo)

**Files:**
- Create: `terraform/gpu-node.tf`
- Modify: `terraform/variables.tf`

**Interfaces:**
- Produces: an Ubuntu VM on pve5 with the RTX 5090 passed through (PCIe), reachable on the LAN — the future vLLM host. **Outside Talos.**

- [ ] **Step 1: Add variables**

Append to `terraform/variables.tf`:

```hcl
variable "gpu_vm_name" {
  description = "Name of the GPU inference VM on pve5."
  type        = string
  default     = "vllm-inference"
}

variable "gpu_vm_node" {
  description = "Proxmox node hosting the GPU (pve5)."
  type        = string
  default     = "pve5"
}

variable "gpu_pci_id" {
  description = "PCI address of the RTX 5090 on pve5 (e.g. 0000:01:00.0). Find with lspci."
  type        = string
}

variable "gpu_vm_ip" {
  description = "Static LAN IP for the GPU VM (CIDR, e.g. 192.168.1.50/24)."
  type        = string
}
```

- [ ] **Step 2: Define the VM with PCIe passthrough**

`terraform/gpu-node.tf`:

```hcl
# GPU inference VM on pve5, OUTSIDE Talos, to isolate vLLM from the fragile
# control plane. Holds the RTX 5090 via PCIe passthrough; LiteLLM reaches it
# over the LAN. Requires IOMMU enabled on pve5 (human prerequisite).
resource "proxmox_virtual_environment_vm" "gpu_inference" {
  name      = var.gpu_vm_name
  node_name = var.gpu_vm_node

  cpu {
    cores = 8
    type  = "host"
  }
  memory {
    dedicated = 16384
  }

  hostpci {
    device = "hostpci0"
    id     = var.gpu_pci_id
    pcie   = true
  }

  disk {
    datastore_id = "local-lvm"
    interface    = "scsi0"
    size         = 100
  }

  network_device {
    bridge = "vmbr0"
  }

  operating_system {
    type = "l26"
  }

  initialization {
    ip_config {
      ipv4 {
        address = var.gpu_vm_ip
      }
    }
  }
}
```

- [ ] **Step 3: Validate (plan-and-stop; NEVER apply autonomously)**

Run: `cd terraform && terraform validate`
Expected: `Success! The configuration is valid.`
Then `terraform plan -out=tfplan` and **stop** — a human reviews and applies. IOMMU enablement on pve5 and host `vfio-pci` binding of the GPU are human prerequisites.

- [ ] **Step 4: Commit**

```bash
git add terraform/gpu-node.tf terraform/variables.tf
git commit -m "feat(gpu): proxmox VM on pve5 with RTX 5090 passthrough for vLLM"
```

### Task 4.2: HUMAN — install vLLM on the GPU VM

Not an agent task; documented here for completeness and captured in the runbook (Task 4.4).

- [ ] **Step 1:** On the VM: install NVIDIA driver + CUDA, then run vLLM serving an OpenAI-compatible endpoint on `:8000`, e.g. `vllm serve Qwen/Qwen3.5-35B-A3B-Instruct --quantization fp8 --max-model-len 16384`.
- [ ] **Step 2:** Verify: `curl http://<gpu_vm_ip>:8000/v1/models` returns the model. Confirm the VM survives reboot via a systemd unit (it is a workstation node and may restart).

### Task 4.3: Register vLLM as a LiteLLM backend + routing (platform repo)

**Files:**
- Modify: `tenants/platform/services/litellm/values.yaml`

**Interfaces:**
- Consumes: vLLM endpoint `http://<gpu_vm_ip>:8000/v1` (Task 4.2).
- Produces: model alias `local-qwen`; routing sends drift-scan/triage to local with Claude fallback; hard RCA stays on `claude-sonnet`.

- [ ] **Step 1: Add the local model + fallbacks**

Add to `proxy_config.model_list` in `tenants/platform/services/litellm/values.yaml`:

```yaml
    - model_name: local-qwen
      litellm_params:
        model: hosted_vllm/Qwen/Qwen3.5-35B-A3B-Instruct
        api_base: http://<gpu_vm_ip>:8000/v1
        api_key: "none"
```

Add fallbacks under `proxy_config.litellm_settings`:

```yaml
    # Local tier down (pve5 rebooted) -> Claude. Claude down -> local.
    fallbacks:
      - local-qwen: ["claude-sonnet"]
      - claude-sonnet: ["local-qwen"]
```

- [ ] **Step 2: Verify render + commit**

Run: `helm template litellm oci://docker.litellm.ai/berriai/litellm-helm --version 0.1.2 -f tenants/platform/services/litellm/values.yaml | grep -q "local-qwen" && echo OK`
Expected: `OK`

```bash
git add tenants/platform/services/litellm/values.yaml
git commit -m "feat(ai-sre): add vllm local-qwen backend + fallbacks"
```

### Task 4.4: Route drift-scan to local tier + hourly CronJob (platform repo)

**Files:**
- Create: `tenants/platform/services/holmes/postInstall/drift-scan-cronjob.yaml`
- Modify: `tenants/platform/tenant.yaml` (ensure holmes `postInstall: true`)
- Create: `scenarios/ai-sre-agent-runbook.md` (infrastructure repo)

**Interfaces:**
- Consumes: Holmes (`local-qwen` model), LiteLLM routing (Task 4.3).
- Produces: hourly cluster drift scan on the free local tier; escalates to Jira/Discord only on findings.

- [ ] **Step 1: Create the CronJob**

`tenants/platform/services/holmes/postInstall/drift-scan-cronjob.yaml`:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: holmes-drift-scan
  namespace: ai-sre
spec:
  schedule: "0 * * * *"           # hourly
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          serviceAccountName: holmes
          containers:
            - name: scan
              image: robustadev/holmes:latest   # match pinned holmes image
              command: ["holmes", "ask"]
              args:
                - "--model=local-qwen"
                - "Scan for cluster drift: pending pods, restart spikes, near-OOM nodes, cert expiry. Report only real anomalies."
              envFrom:
                - secretRef: { name: holmes-llm }
              env:
                - name: OPENAI_API_BASE
                  value: "http://litellm.ai-sre.svc.cluster.local:4000/v1"
```

- [ ] **Step 2: Verify manifest + commit**

Run: `kubectl apply --dry-run=client -f tenants/platform/services/holmes/postInstall/drift-scan-cronjob.yaml`
Expected: `cronjob.batch/holmes-drift-scan created (dry run)`

```bash
git add tenants/platform/services/holmes/postInstall/drift-scan-cronjob.yaml
git commit -m "feat(ai-sre): hourly drift scan on local tier"
```

- [ ] **Step 3: Write the operations runbook (infrastructure repo, this worktree)**

Create `scenarios/ai-sre-agent-runbook.md` documenting: architecture pointer to the spec, how to swap the LLM backend (edit litellm `model_list`), how to rotate keys (Vault paths), what to do when pve5 is offline (fallback is automatic), and how to disable the agent (set holmes `replicas: 0` via values). Commit:

```bash
git add scenarios/ai-sre-agent-runbook.md
git commit -m "docs(ai-sre): operations runbook for the AI-SRE agent"
```

### Task 4.5: PHASE 4 GATE — local tier + fallback proven

- [ ] **Step 1: Drift scan runs on local tier**

Run: `kubectl create job -n ai-sre --from=cronjob/holmes-drift-scan drift-manual`
then `kubectl logs -n ai-sre job/drift-manual`.
Expected: completes; LiteLLM logs show the request hit `local-qwen` (`kubectl logs -n ai-sre deploy/litellm | grep local-qwen`). Claude spend for the scan = $0.

- [ ] **Step 2: Fallback when local is down**

Stop vLLM on pve5 (human), re-run the manual job.
Expected: LiteLLM falls back to `claude-sonnet`; job still completes. Restart vLLM after.

- [ ] **Step 3: Fallback when Claude is down**

Temporarily set an invalid `ANTHROPIC_API_KEY` in Vault, refresh ESO, run a `holmes ask --model=claude-sonnet`.
Expected: routes to `local-qwen`; restore the key after. **Gate passes when both fallback directions work.**

---

## Deferred (NOT in this plan)

- **Phase 5 — auto-remediate whitelist:** a small set of low-risk actions (e.g. restart an OOM pod), evidence-gated, never etcd/control-plane. Requires Holmes write-toolset + an approval mechanism; design separately once v1 has earned trust.
- **Custom Claude-SDK brain:** escape hatch only, if Holmes's loop proves insufficient for a specific fault class. Promote a single fault class, not the whole agent.

---

## Self-Review Notes

- **Spec coverage:** harness (Tasks 1.2, 3.x) ✓; agnostic gateway (0.5, 4.3) ✓; 5090 local tier (4.1–4.5) ✓; detect+explain + PR + Discord (2.x) ✓; triggers event+drift (2.1, 4.4) ✓; Jira/GitHub/Discord outputs (2.2–2.3) ✓; fail-safe/read-only (Global Constraints, 3.1) ✓; replay testing (1.3, 3.3) ✓; deferred auto-remediate + custom brain ✓.
- **Known version placeholders to resolve at execution (not logic gaps):** holmes chart version (`<pinned>`), litellm chart `0.1.2` (confirm latest), exact toolset key names, exact litellm/holmes value casing — each has an explicit verify step (`helm template ... | grep`) before its commit.
- **Cross-repo note:** Phase 4 Task 4.1 + 4.4 Step 3 are in the infrastructure repo; all other tasks are in the platform repo. Use a separate platform worktree.
