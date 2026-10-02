# OpenSearch Fluent Bit addon: actor log attribution

`observability-logs-opensearch` attributes logs by reading `openchoreo.dev/*`
labels off the source pod. Worker pods carry none of those labels — they're
shared across many actors, so labeling one with a single component's
identity would leak every other tenant's logs into that component's view.
Actor logs land in OpenSearch with no `openchoreo_component`/`project`/
`environment` fields as a result, invisible to component-scoped queries.

Substrate's own `actorlog` forwarder writes an actor's stdout to the worker
pod's stdout as JSON, carrying an identity label group
(`logging.googleapis.com/labels` on GCE, `labels` elsewhere) with
`ate.atespace` (the cell namespace, `dp-<cpNs>-<project>-<environment>-<hash>`)
and `ate.actor.name` (`internal/naming.Identity.ActorName`:
`<component>-<environment>-a-<hash10>` once the operator knows the
component/environment names, `a-<hash10>` for actors created before that
naming scheme existed). This chart adds two Fluent Bit filters, scoped to
the `ate-workers` namespace only, that decode that JSON and derive
`openchoreo_component`/`openchoreo_project`/`openchoreo_environment` from
it.

Actors created before the readable-name change only get `project`/
`environment` — there's nothing in their log lines to recover a component
name from, since their `ate.actor.name` is the opaque `a-<hash10>` shape.

## Install

`observability-logs-opensearch` already owns a `fluent-bit` ConfigMap in its
namespace; this chart renders the same ConfigMap with the two extra filters
added, so Helm needs to adopt it first:

```bash
kubectl annotate configmap fluent-bit -n openchoreo-observability-plane \
  meta.helm.sh/release-name=opensearch-fluentbit-addon \
  meta.helm.sh/release-namespace=openchoreo-observability-plane
kubectl label configmap fluent-bit -n openchoreo-observability-plane \
  app.kubernetes.io/managed-by=Helm

helm install opensearch-fluentbit-addon . -n openchoreo-observability-plane
kubectl rollout restart ds/fluent-bit -n openchoreo-observability-plane
```

Set `clusterInstance`, `httpPort`, and `opensearch.host`/`opensearch.port` in
`values.yaml` to match your own `observability-logs-opensearch` install.

## Validate

Deploy any `proxy/substrate-actor` component, call it so it logs something,
then query OpenSearch for the enriched fields:

```bash
kubectl port-forward -n openchoreo-observability-plane svc/opensearch 9200:9200 &
curl -sk -u "$OPENSEARCH_USERNAME:$OPENSEARCH_PASSWORD" \
  "https://localhost:9200/container-logs-*/_search?q=openchoreo_component:<your-component-name>&size=1" \
  | python3 -m json.tool
```

## Known limitations

- Splitting `ate.atespace`/`ate.actor.name` matches a known environment name
  (`development`/`staging`/`production` by default — edit `ENVIRONMENTS` in
  `actor-identity.lua` for other `DeploymentPipeline` names), since
  `cpNs`/project/component names may themselves contain hyphens. `cpNs` and
  project are assumed to be single hyphen-free tokens.
- This chart renders the full `fluent-bit` ConfigMap content, not a patch —
  upgrading `observability-logs-opensearch` to a version with a different
  base config needs this chart's `templates/configmap.yaml` re-synced.
- Only covers this module. `observability-logs-openobserve` needs the same
  fix shape on its own config surface; the cloud-managed backends
  (aws-cloudwatch, gcp-cloudlogging, azure-loganalytics) each need their own
  ingest-time config.
