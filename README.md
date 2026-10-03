# sandboxlab

Create disposable sandbox environments — a shell, a filesystem, a browser — on
demand, in Kubernetes, and throw them away when their time is up.

You create a sandbox from a template, get an address, and use it. It lives for as
long as you asked for and is deleted when that runs out. The whole platform runs
on a throwaway cluster started by a [GitHub Action](action), so there is
nothing to install first and nothing left behind.

```
$ sandbox catalog
agent-infra    AIO Sandbox (browser, shell, MCP) ghcr.io/agent-infra/sandbox:1.11.0
agent-sandbox  Agent Sandbox runtime            sandbox-runtime:latest

$ sandbox create -t agent-infra --name demo --wait
demo  agent-infra  Running
  image      ghcr.io/agent-infra/sandbox:1.11.0
  lifetime   59m left (until 2026-10-01T13:41:12+08:00)
  aio        https://sandbox.example.com/sandbox/sandbox/demo/aio/?key=...

$ sandbox rm demo
deleted demo
```

## Using it

Add this to any repository, from **Actions → Sandboxes → Run workflow**:

```yaml
name: sandboxes
on:
  workflow_dispatch:

jobs:
  sandboxes:
    runs-on: ubuntu-latest
    timeout-minutes: 280
    steps:
      - uses: shaowenchen/sandboxlab/action@main
        with:
          session_hours: '4'
          cloudflare_token: ${{ secrets.CLOUDFLARE_TOKEN }}
```

The run's **Summary** carries a console link and an API key. Open the link,
paste the key, and create a sandbox. See [action/](action) for the inputs and
the tunnel options.

Everything is deleted when the run ends: the cluster, the control plane, and
every sandbox in it.

## The key

There is one key, held by the deployment. It is generated at install and printed
in the environment's summary, and it may do everything: create a sandbox, read
any sandbox, reach any sandbox's ports.

Authorization is "is the key right" and nothing else. A deployment is a personal
or small-team debugging environment rather than a multi-tenant one — there are no
users to keep apart, so there is nothing for a request to be authorized against
beyond the key itself.

The only ceiling is the deployment's own: `SANDBOX_MAX_SANDBOXES` caps how many
sandboxes may exist at once.

## The four interfaces

Everything the API can do is reachable four ways, and all four go through the
same REST interface — so the CLI cannot drift from the console, neither can
drift from the API, and the SDK cannot drift from any of them.

They are kept honest by one document. [`api/openapi.yaml`](api/openapi.yaml)
describes this API, and the SDKs for Go, Python, TypeScript and Java are
generated from it; a change to the spec that is not carried through is a build
failure rather than a client that quietly describes the old API. See
[`sdk/`](sdk/) for the SDKs.

### The console

One page served from inside the binary. It asks for the key, keeps it in
`localStorage` so a reload does not ask again, and then draws the sandboxes, the
templates they can be created from and how long each has left, with a link to
each one's ports.

### The CLI

```bash
go install github.com/shaowenchen/sandboxlab/cmd/sandbox-cli@latest
```

```bash
export SANDBOX_URL='https://<the link>/sandbox'
export SANDBOX_KEY='<the key>'

sandbox catalog                          # what can be created
sandbox create -t agent-infra --name scratch  # create one
sandbox list                             # what is running, and where
sandbox url scratch                      # just the address, for a script
sandbox logs scratch                     # what it has printed
sandbox exec scratch -- ls -la /workspace # run something in it
sandbox cp scratch:/workspace/out.txt .  # take a file out
sandbox renew scratch --ttl 2h           # keep it longer
sandbox rm scratch                       # stop it now
```

`sandbox url` prints one line and nothing else, so a shell can capture it:

```bash
open "$(sandbox url demo --port vnc)"
```

### The API

`GET /api/v1/describe` is the contract and needs no key:

```bash
curl -s https://<the link>/sandbox/api/v1/describe | jq
```

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/catalog` | the templates a sandbox can be created from |
| `POST` | `/api/v1/catalog` | add one: `{document}` as YAML. 409 if the id exists unless `{overwrite: true}`. Not persisted |
| `GET` | `/api/v1/catalog/{id}` | one template |
| `DELETE` | `/api/v1/catalog/{id}` | remove one. Not persisted |
| `POST` | `/api/v1/sandboxes` | create one: `{template, name?, ttl?, env?}` |
| `GET` | `/api/v1/sandboxes` | every sandbox in the deployment |
| `GET` | `/api/v1/sandboxes/{id}` | one, with its addresses and remaining time |
| `DELETE` | `/api/v1/sandboxes/{id}` | delete one |
| `POST` | `/api/v1/sandboxes/{id}/renew` | reset its expiry |
| `GET` | `/api/v1/sandboxes/{id}/logs` | the tail of its output |
| `POST` | `/api/v1/sandboxes/{id}/exec` | run a command in it and wait; `{command, stdin?, cwd?, timeout?}` |
| `GET` | `/api/v1/sandboxes/{id}/files` | read a file: `?path=/abs/path` |
| `PUT` | `/api/v1/sandboxes/{id}/files` | write a file: `?path=/abs/path`, `{content, encoding?, createParents?}` |
| `GET` | `/api/v1/overview` | counts by state and template |
| `GET` | `/sandbox/{id}/{port}/` | proxy to a sandbox's own port |

Every route takes the key in `Authorization: Bearer <key>` or `X-Sandbox-Key`.
The `/sandbox/` routes also take `?key=`, because a browser navigation cannot set
a header — which is what makes the addresses the console and the CLI print
clickable.


### The SDK

[`sdk/`](sdk/) holds a typed client for Go, Python, TypeScript and Java, all
generated from [`api/openapi.yaml`](api/openapi.yaml) — the same document the
routes above come from.

```go
client, err := sdk.New(sdk.Options{
    BaseURL: "https://sandboxlab.example.com/sandbox",
    Key:     os.Getenv("SANDBOXLAB_KEY"),
})
if err != nil {
    return err
}
created, err := client.CreateSandboxWithResponse(ctx, sdk.CreateSandboxRequest{
    Template: "agent-infra", Name: "myshop", TTL: ptr("30m"),
})
```

The Go package has no dependencies; Python needs `urllib3` and `pydantic`,
TypeScript needs nothing at all, and Java needs Jackson. The generated code is
committed, so consuming it does not require a code generator — see
[`sdk/README.md`](sdk/README.md) for a worked example in each language.

```sh
make sdk     # regenerate after editing the spec
```

## Templates

A template says what image to run, what ports it serves, and how long a sandbox
may live. Templates are compiled into the control plane, and more can be added
to a **running** deployment without a release — from the console, the CLI or the
API:

```bash
sandbox catalog add ./tool.yaml       # add or replace one
sandbox catalog rm tool               # remove one
```

They are held in memory and are **not persisted**: the control plane returns to
the templates compiled into it when it restarts. That is the trade this makes
for needing no store — a template added to debug something does not have to be
cleaned up, and a change worth keeping is a change worth a release.

```yaml
id: tool
title: Internal tool
image: registry.example.com/me/tool:1.2.3

ports:
  - name: web
    port: 8080

ttlDefault: 30m
ttlMax: 4h
```

The three that ship:

| | |
|---|---|
| **agent-infra** | [AIO Sandbox](https://github.com/agent-infra/sandbox) — a browser, a shell, a filesystem, an MCP server and VS Code in one container, all reached through the web UI on one port. The one to reach for when the question is "can this be done at all". |
| **agent-sandbox** | The [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) reference runtime: a small server that executes commands over HTTP. |
| **opensandbox** | [alibaba/OpenSandbox](https://github.com/alibaba/OpenSandbox)'s code-interpreter image — Python, Java, Go and Node preinstalled, for running model-generated code. |

Two of these need a word more. The `agent-sandbox` image has to be **built and
pushed first**: agent-sandbox is a Kubernetes CRD and controller, not an image,
so its quickstart builds one locally. Build it from the project's
[`examples/python-runtime-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/python-runtime-sandbox),
push it where your cluster can pull, and point the template's `image` at it. A
sandbox created before that is a pod that cannot pull its image.

`opensandbox` declares no port, and that is deliberate: OpenSandbox's image
serves nothing until its own control plane injects an `execd` daemon into it.
This deployment reaches a sandbox through its own exec and file API instead, so
the template is a workspace — Python, Java, Go and Node to hand — rather than a
service to open.

The `agent-infra` image runs an unconfined seccomp profile in its own quickstart.
A template cannot set that (`internal/k8s` builds a plain container), so on a
cluster that enforces a restrictive default profile, allow it for the sandbox
namespaces at the cluster level.

A template with no ports serves no URL — a workspace to run things in rather than
a service to open. It is reached through the API:

```bash
sandbox exec scratch -- python3 -c 'print("hi")'
sandbox cp ./setup.sh scratch:/workspace/setup.sh
sandbox cp scratch:/workspace/report.csv .
```

`exec` runs one command and waits. A shell is something you ask for by naming
one, so `-- sh -c 'a | b'` is how you get a pipeline and everything else is
executed directly. Its exit status becomes the command's own, which makes
`sandbox exec x -- test -f /workspace/ready` a condition in a script rather
than something to parse.

`cp` moves a file in whichever direction the side naming a sandbox says. `-`
on the local side means stdin or stdout, so it composes with a pipe.

This is also how an agent reaches a sandbox: the same two primitives over HTTP
at `POST /api/v1/sandboxes/{id}/exec` and `GET`/`PUT /api/v1/sandboxes/{id}/files`,
with the deployment's key. A sandbox with a port is a URL you can open, and one
without is a machine you can run things on — which is what a workspace was
always meant to be.

## How it works

```
GitHub runner
├── kind cluster (created for the run, deleted with it)
│   ├── istio-system/          the ingress gateway
│   ├── ops-system/            the control plane
│   └── sbx-<id>/              one namespace per sandbox
│       ├── Deployment         the sandbox's image
│       ├── Service
│       ├── ResourceQuota      what it may use
│       └── NetworkPolicy      who may reach it
└── cloudflared / ngrok
      https://<host>/sandbox
        ├── /api/v1/*          the API
        ├── /                  the console
        └── /sandbox/<id>/...  into a sandbox's own ports
```

**A sandbox is a namespace.** That one decision shapes everything else. The
quota and the network policy that bound it are namespace objects; listing the
sandboxes is a label query on the namespaces; and deleting is one call that
reclaims the container, the service, the volume and anything else created inside
it, with no list to keep in step and nothing to miss.

It also means an id is a namespace name, which is why ids are lowercase,
dash-separated and at most 40 characters — an unusual rule that is exactly the
price of the clean deletion.

**Nothing is persisted.** Every sandbox the API reports is read back from the
cluster, and a sandbox's expiry is an annotation on its own namespace rather than
something a process remembers. A control plane that restarts loses no schedule
and forgets no sandbox — which is what lets this run on a throwaway runner, where
the process and the cluster disappear together.

**The deployment's key is a Secret in the control plane's namespace**, like
everything else here: the cluster is the record, so a restart loses no credential
and there is no database to back up. It is also the right object — a key is a
credential, and a Secret is where a credential belongs.

**Sandboxes are reached through the control plane** at `/sandbox/<id>/<port>/`,
not through an ingress object of their own. Three things follow: creating a
sandbox is a cluster write and nothing else, so it cannot half-fail somewhere a
delete cannot clean up; there is no per-sandbox hostname to arrange; and the one
hostname a tunnel gives you serves the console and every sandbox at once.

## Running it yourself

The control plane is an ordinary Go binary and a Helm chart. The action wires
them together on a runner; the pieces are usable without it.

```bash
make build          # bin/sandbox and bin/sandbox-cli

helm install sandbox ./charts/sandbox \
  --namespace ops-system --create-namespace \
  --set publicURL=https://sandbox.example.com \
  --set istio.enabled=true --set istio.gateway=istio-system/ingressgateway
```

See [charts/sandbox/README.md](charts/sandbox/README.md) for the values, the
RBAC it needs, and what it does not create. The chart is also published as a
Helm repository at <https://www.chenshaowen.com/sandboxlab>, where that same
README is the page you land on:

```bash
helm repo add sandboxlab https://www.chenshaowen.com/sandboxlab
helm install sandbox sandboxlab/sandbox --namespace ops-system --create-namespace
```

To run the whole environment by hand — a cluster, a gateway, the control plane —
without GitHub Actions:

```bash
# Needs kind, kubectl, istioctl, helm and docker on PATH.
SANDBOXLAB_PUBLIC_HOST=localhost SANDBOXLAB_SESSION_HOURS=0 \
  ./hack/environment.sh
```

Set `SANDBOXLAB_PUBLIC_HOST` to skip the tunnel entirely. That is also how
CI runs it: a build should not depend on a public tunnel being granted.

## Developing

```bash
make check        # gofmt, vet, tests
make helm-check   # render the chart and assert what a deployment needs
hack/action-check.sh   # the action's inputs reach the script
```

`make check` needs no cluster: every test runs against a fake clientset. The
end-to-end job in [.github/workflows/ci.yml](.github/workflows/ci.yml) is the one
that starts a real environment, and it is where the cluster behaviour is proved.

## Publishing

The Helm chart and the documentation are published together to a `gh-pages`
branch by [.github/workflows/pages.yml](.github/workflows/pages.yml), on every
push to the default branch and on every tag. They share a branch, so they share
a workflow: the packaged chart is `helm repo add`-able from that branch, and the
page you land on from it is the chart's README, rendered.

```bash
make chart-package PAGES=./pages   # helm package + helm repo index
make docs PAGES=./pages            # render the markdown into the site
```

`make docs` reads the repository's own markdown rather than a second copy kept
in step by hand, and fails on a link that would be dead on the site — so the
site cannot disagree with the repository.

Most of the tests are over the seams rather than inside the packages — the API
against a stub, the proxy through the real router, and an integration test that
wires the whole stack to a fake cluster. That last one is what caught the public
path being joined onto a sandbox's own, and the base path being applied twice:
both were internally consistent on each side and wrong together.

## License

See [LICENSE](LICENSE).
