# sandboxlab

Create disposable sandbox environments — a shell, a filesystem, a browser — on
demand, in Kubernetes, and throw them away when their time is up.

You create a sandbox from a template, get an address, and use it. It lives for as
long as you asked for and is deleted when that runs out. The whole platform runs
on a throwaway cluster started by a [GitHub Action](action), so there is
nothing to install first and nothing left behind.

```
$ sandbox catalog
all-in-one     Linux desktop (browser) linuxserver/webtop:ubuntu-xfce
code-server    VS Code (browser)      codercom/code-server:latest
node           Node sandbox           node:22-slim
python         Python sandbox         python:3.12-slim

$ sandbox create -t all-in-one --name demo --wait
demo  all-in-one  Running
  image      linuxserver/webtop:ubuntu-xfce
  lifetime   59m left (until 2026-10-01T13:41:12+08:00)
  desktop    https://sandbox.example.com/sandbox/sandbox/demo/desktop/?key=...

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
sandbox create -t python --name scratch  # create one
sandbox list                             # what is running, and where
sandbox url scratch                      # just the address, for a script
sandbox logs scratch                     # what it has printed
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
| `POST` | `/api/v1/sandboxes` | create one: `{template, name?, ttl?, env?}` |
| `GET` | `/api/v1/sandboxes` | every sandbox in the deployment |
| `GET` | `/api/v1/sandboxes/{id}` | one, with its addresses and remaining time |
| `DELETE` | `/api/v1/sandboxes/{id}` | delete one |
| `POST` | `/api/v1/sandboxes/{id}/renew` | reset its expiry |
| `GET` | `/api/v1/sandboxes/{id}/logs` | the tail of its output |
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
    Template: "all-in-one", Name: "myshop", TTL: ptr("30m"),
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
may live. They ship in the binary and can be extended by a directory of files or
a ConfigMap, so an environment can be given a new one without a release.

```yaml
id: all-in-one
title: Linux desktop (browser)
description: >-
  A full Linux desktop in a browser: window manager, browser, terminal and
  filesystem. Nothing to install first.
image: linuxserver/webtop:ubuntu-xfce

ports:
  - name: desktop
    port: 3000

resources:
  cpu: "2"
  memory: 4Gi

ttlDefault: 1h
ttlMax: 8h
```

The four that ship:

| | |
|---|---|
| **all-in-one** | A full Linux desktop in a browser — window manager, browser, terminal, filesystem. `linuxserver/webtop`, the lightest flavor it ships. The one to reach for when the question is "can this be done at all". |
| **python** | Python 3.12 that stays up, for scripts and pip. |
| **node** | Node 22 on the same terms. |
| **code-server** | VS Code in a browser, for work that is editing rather than running. |

All four are public Docker Hub images, deliberately: nothing here has to be
pulled from a registry with an account, so an environment works the moment it
starts.

Templates with no ports — `python` and `node` — serve no URL, because their image
serves nothing. They are a workspace to run things in rather than a service to
open, and they are reached by going into the cluster:

```bash
kubectl -n sbx-scratch exec -it deploy/sandbox -- python3
```

An API for that — a `sandbox exec` that shells in without needing cluster
credentials — is the natural next step and is not built yet. Today a sandbox with
a port is reachable by anyone holding the key, and one without is reachable by
whoever can reach the cluster.

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
