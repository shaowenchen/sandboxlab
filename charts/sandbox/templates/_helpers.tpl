{{/*
Helpers. The names follow the usual chart conventions so an override behaves as
expected.
*/}}

{{- define "sandbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sandbox.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sandbox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sandbox.labels" -}}
helm.sh/chart: {{ include "sandbox.chart" . }}
app.kubernetes.io/name: {{ include "sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "sandbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The namespace the control plane runs in, which is not the release
     namespace unless the values say so. */}}
{{- define "sandbox.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "sandbox.serviceAccountName" -}}
{{- printf "%s-control-plane" (include "sandbox.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sandbox.cleanupServiceAccountName" -}}
{{- printf "%s-cleanup" (include "sandbox.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sandbox.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
The Secret the key comes from: one the operator named, or the one this chart
creates.

A named Secret is the way a key that the release should not carry is supplied.
It has to be in the same namespace and carry the same `api-key` field, because
the Deployment names it as one object — which is why `existingSecret` and
`apiKey` together are refused rather than merged.
*/}}
{{- define "sandbox.apiKeySecretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-apikey" (include "sandbox.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
The API key.

Sources, in order: a Secret the operator named, an explicit value, the Secret
this release already created, then a fresh random one. The second-to-last case
is the one that matters — a Secret's contents are rendered once and then kept,
but the template is re-rendered on every upgrade, and on an upgrade `lookup` is
the only way to see what is already there. Without it, every `helm upgrade`
would mint a new key and invalidate every client that had been given the old
one.

A named Secret is deliberately not read here. The value is the operator's and it
lives in the cluster; this chart names the Secret and the Deployment reads it
there, so the value never enters the release. It also could not be trusted if it
were read: `randAlphaNum` below is not memoized, so two calls to this helper can
return two different keys, and anything showing "the key" would have to be sure
which one it printed. secret.yaml's gate, not this helper, is where the two
settings together are refused.

On `helm template` (which the tests and CI run) `lookup` returns nothing, so a
key is generated and printed into the manifest — which is exactly what a
`helm install` does the first time, so the rendered output is representative.
*/}}
{{- define "sandbox.apiKey" -}}
{{- $name := printf "%s-apikey" (include "sandbox.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" (include "sandbox.namespace" .) $name -}}
{{- if .Values.apiKey -}}
{{- .Values.apiKey -}}
{{- else if and $existing (hasKey ($existing.data | default dict) "api-key") -}}
{{- index $existing.data "api-key" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 | lower -}}
{{- end -}}
{{- end -}}
