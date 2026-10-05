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
naming scheme existed).

Neither string is safe to split on `-`: component, project, and environment
names are ordinary Kubernetes names, and Kubernetes does not forbid hyphens
inside one (only leading/trailing). A component could be named
`my-app-prod`, indistinguishable from a split by position alone. So this
addon never splits either string. oc-substrate's `ActorTemplate` controller
already knows, with no ambiguity, which atespace and actor name belong to
which component/project/environment/namespace/UID — it publishes that
mapping, keyed by the exact opaque string, into an
`actor-identity-attribution` ConfigMap. This chart mounts that ConfigMap
into fluent-bit and adds two filters, scoped to the `ate-workers` namespace
only: one decodes the actor's JSON log line, the other looks up
`ate.atespace` and `ate.actor.name` verbatim against the mounted files and
writes whatever it finds — `openchoreo_component`/`openchoreo_project`/
`openchoreo_environment` (names, for display) and the real
`openchoreo.dev/{component,project,environment}-uid` and
`openchoreo.dev/namespace` Kubernetes labels (what
`observability-logs-opensearch`'s own query actually filters by). No
parsing, so no hyphen ever matters.

Actors created before the readable-name change get none of this — their
`ate.actor.name` is the opaque `a-<hash10>` shape, which the controller
never publishes an entry for.

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

- This chart renders the full `fluent-bit` ConfigMap content, not a patch —
  upgrading `observability-logs-opensearch` to a version with a different
  base config needs this chart's `templates/configmap.yaml` re-synced.
- Only covers this module. `observability-logs-openobserve` needs the same
  fix shape on its own config surface; the cloud-managed backends
  (aws-cloudwatch, gcp-cloudlogging, azure-loganalytics) each need their own
  ingest-time config.
- A component created before the operator had
  `--attribution-configmap-namespace` set won't have a ConfigMap entry until
  its `ActorTemplate` next reconciles (creation, spec change, or the
  controller's periodic resync — a few minutes at most, not a restart away).
- The ConfigMap only ever gains entries; deleting a component removes its
  own actor-name entry but atespace/project/environment entries are left for
  others still using them. Harmless (never looked up again), just not
  pruned to zero.
