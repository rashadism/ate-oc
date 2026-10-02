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

Names alone don't make a component's logs show up in the console, though.
`observability-logs-opensearch`'s own query always filters by
`openchoreo.dev/namespace` — the project's Kubernetes namespace, a separate
value from the project's *name* even though the two are often equal — and,
when scoped further, by `openchoreo.dev/{component,project,environment}-uid`.
Both are real values a log line can't derive by string-splitting: a
namespace isn't encoded anywhere in the actor identity, and a UID isn't
recoverable from a name at all. oc-substrate's `ActorTemplate` controller
publishes both (namespace per project, UID per component/project/environment)
into an `actor-identity-attribution` ConfigMap; this addon mounts that
ConfigMap into the fluent-bit pod so the same Lua filter can look up the real
value for whatever name it already recovered, and write it as the actual
label key the query expects.

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

Mount the attribution ConfigMap into fluent-bit by adding one more volume to
the base release's own `fluent-bit` values and re-running its `helm upgrade`
(this is the official `fluent/fluent-bit` chart underneath
`observability-logs-opensearch`, which already supports `extraVolumes`/
`extraVolumeMounts` — no DaemonSet patch, no new ownership):

```yaml
fluent-bit:
  extraVolumes:
    - name: attribution
      configMap:
        name: actor-identity-attribution
        optional: true
  extraVolumeMounts:
    - name: attribution
      mountPath: /etc/attribution
      readOnly: true
```

`optional: true` so fluent-bit still starts before the operator's first
reconcile creates the ConfigMap, or if attribution isn't enabled on the
operator at all (`--attribution-configmap-namespace` unset). Requires
oc-substrate running with `--attribution-configmap-namespace` pointed at
this namespace.

## Validate

Deploy any `proxy/substrate-actor` component, call it so it logs something,
then query OpenSearch for the enriched fields:

```bash
kubectl port-forward -n openchoreo-observability-plane svc/opensearch 9200:9200 &
curl -sk -u "$OPENSEARCH_USERNAME:$OPENSEARCH_PASSWORD" \
  "https://localhost:9200/container-logs-*/_search?q=openchoreo_component:<your-component-name>&size=1" \
  | python3 -m json.tool
```

To confirm the namespace/UID lookups specifically (what the console's own log
query actually needs), check the same document for
`kubernetes.labels.openchoreo_dev/namespace` and
`kubernetes.labels.openchoreo_dev/component-uid`, and compare the latter
against `kubectl get component <name> -o jsonpath='{.metadata.uid}'`. If
either field is missing, check in order: the attribution ConfigMap has the
entry
(`kubectl get cm actor-identity-attribution -n openchoreo-observability-plane -o yaml`),
the fluent-bit pod actually mounted it (`kubectl exec` into it and
`ls /etc/attribution`), and the operator was started with
`--attribution-configmap-namespace` set.

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
- UID attribution depends on the same name recovery as the fields above, so
  it inherits the same hyphen-ambiguity limitation. A component created
  before the operator had `--attribution-configmap-namespace` set won't have
  a ConfigMap entry until its `ActorTemplate` next reconciles (creation,
  spec change, or the controller's periodic resync — a few minutes at most,
  not a restart away).
- The ConfigMap only ever gains entries; deleting a component removes its
  own entry but project/environment entries are left for others still using
  them. Harmless (never looked up again), just not pruned to zero.
