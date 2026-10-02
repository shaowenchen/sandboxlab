# sandboxlab chart

Installs the sandbox control plane: the API, the console, and the proxy that
reaches into a sandbox.

A **sandbox** here is a namespace. Creating one is a cluster-scoped write, it is
deleted as a unit when its time is up, and it holds nothing the control plane
needs to remember — which is why this chart has no database, no PersistentVolume
and no state to back up.

## Installing

From the published repository:

```bash
helm repo add sandboxlab https://www.chenshaowen.com/sandboxlab
helm repo update
helm install sandbox sandboxlab/sandbox \
  --namespace ops-system --create-namespace \
  --set publicURL=https://sandbox.example.com
```

Or from a checkout:

```bash
helm install sandbox ./charts/sandbox \
  --namespace ops-system --create-namespace \
  --set publicURL=https://sandbox.example.com
```

The control plane lands in **`ops-system`** by default (`namespaceOverride`).
That is not the release namespace, deliberately: this is infrastructure, and the
things it manages — the sandboxes — get namespaces of their own.

Read the key:

```bash
kubectl -n ops-system get secret sandbox-apikey \
  -o jsonpath='{.data.api-key}' | base64 -d; echo
```

`helm install` prints the same instructions, filled in, in its notes.

## Prerequisites

| | |
|---|---|
| **A cluster** | Any. Nothing here is provider-specific. |
| **Cluster-scoped RBAC to create** | A sandbox is a namespace, so the control plane needs `create`/`delete` on namespaces cluster-wide. See [RBAC](#rbac). |
| **Something to publish with** | An Istio gateway, an Ingress, or a tunnel. Or nothing, and a port-forward. |

## Reaching it

Three routes reach the deployment, and you pick one:

**A tunnel** — what the [sandboxlab action](../../action/README.md) does. `publicURL` is
the tunnel's hostname, `basePath` is set, and `istio.enabled=true` writes the
one VirtualService that puts the console and every sandbox behind it.

**An Ingress** — `ingress.enabled=true` and `ingress.host`. Point it at the
control plane's Service and set `publicURL` to the same host.

**A port-forward** — nothing to configure. Leave `publicURL` empty and the API
reports relative addresses:

```bash
kubectl -n ops-system port-forward svc/sandbox 8080:80
```

### The base path

`basePath` is a path prefix the whole deployment is served under: `/sandbox`
means the console is at `https://host/sandbox` and a sandbox at
`https://host/sandbox/sandbox/<id>/<port>/`. Leave it empty to serve at the
root.

It is not cosmetic. The environment the sandboxlab action builds publishes
everything on one hostname, and the path is what tells the console's own routes
from an app's — so the base path is how the control plane and a sandbox can
share an address without a second hostname.

## RBAC

The control plane's ClusterRole grants, cluster-wide:

- `namespaces`: get, list, watch, create, update, patch, delete
- `deployments`, `services`, `resourcequotas`, `limitranges`, `pods`,
  `configmaps`, `persistentvolumeclaims`, `pods/log`, `networkpolicies`: the
  full set, inside the namespaces it creates
- `secrets`: **get only**. Nothing in the process writes a Secret — the key is
  created at install and read from the environment — so the grant stopped at
  read rather than being widened to the set above out of habit.
- `pods/exec`: get, create. This is the one that lets `sandbox exec` and the
  file endpoints run anything inside a sandbox, so it is worth knowing it is
  there: whoever holds the deployment's key can execute in any sandbox this
  ServiceAccount can reach.

`delete` on namespaces is the one worth reviewing: deleting a namespace
reclaims everything in it. That is the design — one operation, nothing to miss —
and it is also the most destructive thing this component can do.

`rbac.create=false` skips the ClusterRole and ClusterRoleBinding if you supply
your own; the chart then expects a ServiceAccount named
`<release>-sandbox-control-plane` to already have the grants.

## The catalog

The templates a sandbox can be created from come from two places:

1. **Built in**, compiled into the image: `all-in-one`, `python`, `node`,
   `code-server`.
2. **This release**, through `catalog` (inline) or `catalogDir` (a mounted
   directory). An id in either **replaces** the built-in of that name; it does
   not merge with it.

```yaml
catalog:
  internal-tool.yaml: |
    id: internal-tool
    title: Internal tool environment
    image: registry.example.com/internal/tool:1.2.3
    ports:
      - name: web
        port: 8080
    ttlDefault: 30m
    ttlMax: 4h
```

`disableTemplates` removes templates by id from the final catalog — a misspelled
id there is a startup error rather than a template that quietly stays listed.

## Uninstalling

`helm uninstall` removes the control plane **and the sandboxes it created**. A
`pre-delete` Job deletes every namespace carrying
`app.kubernetes.io/managed-by=sandboxlab`, which is every sandbox.

That Job exists because a sandbox is a namespace of its own: nothing in the
release owns it, so without it an uninstall would leave every sandbox behind —
running, costing resources, and reachable by nobody. Set `cleanup.enabled=false`
to keep them, and delete them by hand.

## Values

| Value | Default | What it is |
|---|---|---|
| `namespaceOverride` | `ops-system` | Where the control plane runs. |
| `image.repository` / `image.tag` | chart appVersion | The control plane image. |
| `apiKey` | generated | The key every call carries. Empty generates one and reuses it across upgrades. |
| `publicURL` | — | The address the API reports sandbox URLs under. Empty reports relative ones. |
| `basePath` | — | A path prefix the deployment is served under. |
| `sandboxNamespacePrefix` | `sbx-` | Prefix of a sandbox's namespace. |
| `defaultTTL` / `maxTTL` | `1h` / `8h` | Sandbox lifetimes. |
| `maxSandboxes` | `0` (no ceiling) | How many may exist at once. |
| `reapInterval` | `30s` | How often expired sandboxes are collected. |
| `execTimeout` | `1m` | How long a command run in a sandbox may take by default. |
| `execTimeoutMax` | `10m` | The longest a caller may ask for. |
| `maxFileBytes` | `2097152` | The largest file the API reads or writes. |
| `dataPlane` | `true` | Serve `/sandbox/<id>/<port>/` by proxying into a sandbox. |
| `catalog` / `catalogDir` | — | Templates to add or replace. |
| `disableTemplates` | `[]` | Template ids to leave out. |
| `istio.enabled` / `istio.gateway` | `false` / — | Write the VirtualService. The gateway must already exist. |
| `ingress.enabled` / `ingress.host` | `false` / — | Write an Ingress instead. |
| `cleanup.enabled` | `true` | Delete the sandboxes on uninstall. |
| `replicaCount` | `1` | One, because each replica runs its own reaper. |

`kubectl explain` and `values.yaml` are the exhaustive sources; every field is
commented where it is defined.

## What it does not create

- **A Gateway.** An Istio gateway is shared by everything on a cluster; the
  chart points at one (`istio.gateway`) and fails to render without it rather
  than installing a route that attaches to nothing.
- **A registry or an object store.** The control plane has no state to keep and
  no images to build.
- **TLS.** Terminate it at the tunnel, the load balancer, or the Ingress.
