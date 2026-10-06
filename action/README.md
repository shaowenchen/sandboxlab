# sandboxlab action

Start a complete sandbox environment on a GitHub runner — a Kubernetes cluster,
an Istio gateway and the control plane, built from this commit — hand yourself a
link, and create sandboxes from it.

The point is that a sandbox needs real infrastructure before it can do anything:
a cluster to run in and a gateway to be reached through. This action assembles
all of it on a throwaway kind cluster, so the first thing you do is not "install
a cluster" but "create a sandbox".

## Using it from another repository

```yaml
name: sandboxes
on:
  workflow_dispatch:

jobs:
  sandboxes:
    runs-on: ubuntu-latest
    # A little longer than the environment's own lifetime, so it stops itself
    # and shuts down cleanly rather than being killed at the runner's ceiling.
    timeout-minutes: 280
    steps:
      - uses: shaowenchen/sandboxlab/action@main
        with:
          session_hours: '4'
          # Only needed for the default named tunnel. Set domain to empty to use
          # a quick tunnel instead, which needs no account.
          cloudflare_token: ${{ secrets.CLOUDFLARE_TOKEN }}
```

That is the whole workflow. Open the run's **Summary** for the console link and
the API key, then create a sandbox:

```bash
export SANDBOX_URL='https://<the link>/sandboxlab'
export SANDBOX_KEY='<the key>'

sandbox catalog
sandbox create -t agent-infra --name demo --wait
sandbox url demo
```

The `sandbox` CLI is from [this repository](../README.md); the API is also
directly usable, and `GET /api/v1/describe` is the contract — it needs no key.

## What it starts

| | What it is |
|---|---|
| **kind cluster** | A throwaway Kubernetes cluster, created for this run and deleted with it. |
| **The control plane** | Built from this commit's Dockerfile and loaded into the cluster, installed with [this repository's Helm chart](../charts/sandbox/README.md). Testing the commit under test is the point; a published `latest` would test yesterday's. |
| **metrics-server** | The cluster's resource metrics API. It is what makes a sandbox's CPU and memory visible — in the console's Metrics panel and through `GET /api/v1/sandboxes/{id}/usage`; without it both report usage as unavailable. Pinned to `v0.7.2`, with the two flags a kind cluster's kubelets need. |
| **Istio** | The ingress gateway the console and every sandbox are published through, plus the `Gateway` resource `istioctl install` deliberately does not write. |
| **cloudflared** | A named tunnel, published at `domain`. Set `domain` to empty for a quick tunnel instead, or `tunnel: ngrok` to use ngrok. |

Everything on one hostname, and the base path is what tells the console's own
routes from a sandbox's: the console at `<domain>/sandboxlab`, a sandbox at
`<domain>/sandboxlab/sandbox/<name>/<port>/`.

## Inputs

| Input | Default | Description |
|---|---|---|
| `api_key` | empty → generated | API key for this environment. Empty means the environment generates one. Printed in the summary either way, because it is the deliverable. |
| `session_hours` | `4` | How long the environment may run. `0` means no self-imposed limit, bounded by the job's timeout. |
| `tunnel` | `cloudflare` | `cloudflare` (no account needed) or `ngrok`. |
| `cloudflare_token` | — | Token of a named Cloudflare tunnel; empty starts a quick tunnel. |
| `domain` | `sandboxlab-1.chenshaowen.com` | The domain the console is served under. Named by default, and explained below. |
| `ngrok_token` | — | ngrok authtoken; required when `tunnel` is `ngrok`. |
| `base_path` | `/sandboxlab` | The path the deployment is served under. |
| `default_ttl` | `1h` | How long a sandbox lives when it asks for nothing else. |
| `max_ttl` | `8h` | The longest a sandbox may live. |

Only `api_key` and `cloudflare_token` are worth passing from a secret: the key is
generated when left empty, so it needs no configuration unless you want a
particular one, and the token is a credential and never a plain input.

### A fixed key for the debugger

`gh workflow run debugger.yml` generates a fresh key every run and prints it in
the Summary. There are two ways to pin one instead, so a `SANDBOX_KEY` in a
shell, or a saved console session, keeps working across runs:

- Set the repository secret **`SANDBOXLAB_API_KEY`**. Every run then uses it.
- Pass the workflow's **`api_key` input** at dispatch
  (`gh workflow run debugger.yml -f api_key=...`). This is how an automated
  caller hands over the key it will call the environment with, without the two
  sides having to be configured with the same value out of band.

The dispatch input wins when it is set; otherwise the secret pins the key; with
both empty the environment generates one exactly as before.

`api_key` is deliberately a plain workflow input rather than a secret: whoever
starts the run is choosing a key for it, and can read it back from the run's
Summary anyway. The **secret** path is the one to use when the key must not
appear in the dispatch at all — an input is visible to anyone who can read the
run.

## The domain, and the named tunnel it needs

`domain` defaults to `sandboxlab-1.chenshaowen.com`, and it is used as given: it
becomes the control plane's `publicURL`, which is both where the console lives
and what every sandbox's address is built from.

The action takes it as free text — a composite action cannot declare a dropdown,
because `type: choice` is a `workflow_dispatch` feature — but the workflow that
offers this one, [`.github/workflows/debugger.yml`](../.github/workflows/debugger.yml),
turns it into a choice of the hostnames that have a Cloudflare public hostname
behind the tunnel. Anything else resolves to nothing and answers 530.

This is app configuration, not tunnel configuration. Nothing is passed to
`cloudflared` on this path — a named tunnel already knows its ingress, because
you configured it.

It only works with a **named** tunnel — the one whose hostname and ingress live
in your Cloudflare dashboard. A quick tunnel is assigned a random
`trycloudflare.com` hostname by Cloudflare and cannot be given another, so the
console could not be served under the domain you named; the ngrok path here does
not pass the flag a reserved domain needs either. Both combinations are refused
when the run starts, rather than after a wait for a hostname that was never
coming.

A named tunnel keeps its hostname in its ingress, and Cloudflare never tells the
connector its own name, so the environment cannot discover the address the
console should be served under. That is why the domain is declared rather than
found, and why it has a default at all: the value has to come from a person, and
naming it once is better than naming it on every run.

**One thing this does not do: create the ingress.** Point the hostname at the
tunnel in the Cloudflare dashboard first, or the address will resolve to a tunnel
that routes nothing.

Setting `domain` to empty goes back to a quick tunnel: one is assigned a random
`trycloudflare.com` hostname, needs no account, and that hostname serves the
console. The link appears in the summary.

## The two things most likely to go wrong

**A run with no `cloudflare_token` stops.** The default `domain` needs a named
tunnel, and a quick tunnel cannot be given another hostname — so the run refuses
the combination rather than publishing an address that does not match the domain
it was told to serve. Set `cloudflare_token`, or set `domain` to empty.

**A quick tunnel is for trying things.** It carries no SLA and its hostname is
minted per connection, so a restart gives a different link. That is exactly right
for a session you open now and discard, and wrong for anything you keep — which is
why the default is a named domain rather than one of these.

## What it costs

Roughly ten minutes to come up, most of it Istio. Everything is deleted when the
run ends: the cluster, the control plane, every sandbox. Nothing survives, which
is the point.

## Implementing it yourself

Nothing here is specific to GitHub Actions except the workflow file. The pieces
are ordinary scripts, and they are documented where they are:

| Path | What it does |
|---|---|
| [action/action.yml](action.yml) | The composite action: installs kind, kubectl, istioctl, helm and a tunnel agent, then runs the script. |
| [hack/environment.sh](../hack/environment.sh) | The whole environment, in order. Set `SANDBOXLAB_PUBLIC_HOST` to skip the tunnel and use a hostname you already have, or `SANDBOXLAB_DOMAIN` to name the domain a named tunnel serves. |
| [hack/summary.sh](../hack/summary.sh) | Publishes the link, the key and the catalog to the job summary. |

## License

See [LICENSE](../LICENSE).
