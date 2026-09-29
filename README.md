# oc-substrate

Run OpenChoreo components on [Agent Substrate](https://github.com/agent-substrate/substrate): snapshot-backed actors multiplexed onto shared warm workers, using only OpenChoreo's existing abstractions.

## Cluster prerequisites

Agent Substrate identifies pods with Kubernetes' `PodCertificateRequest`/`ClusterTrustBundle` APIs (`certificates.k8s.io/v1beta1`), which need explicit enablement:

- **Kubernetes 1.37 or newer** on every node that will run a substrate component. `PodCertificateProjection` is a kubelet feature gated through 1.36 and GA in 1.37; a node started before the beta APIs were turned on never gets it, even once the control plane serves them. On a 1.36 cluster, replace (don't just upgrade in place) any node pool created before enabling the APIs.
- **The beta APIs enabled**, even on 1.37+: substrate's controllers use the `v1beta1` types specifically, not the GA `v1` ones a 1.37 control plane serves by default. On GKE:
  ```
  gcloud container clusters create <name> ... \
    --enable-kubernetes-unstable-apis=certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles
  ```
- **Auto-upgrade off, and no spot/preemptible nodes, on any node pool that runs workers.** A worker pod's actors get a 30-minute grace window to suspend when their pod is deleted; one still running when that window closes goes to a terminal `CRASHED` state with no recovery path. Auto-upgrade is the main trigger since GKE enables it by default on Google's own schedule, not yours.

## Install

Install in this order: OpenChoreo, then Agent Substrate, then this operator, then a sample component.

### 1. OpenChoreo

A running OpenChoreo control plane and data plane are a prerequisite. Follow OpenChoreo's own [quick start guide](https://openchoreo.dev/docs/getting-started/quick-start-guide).

### 2. Agent Substrate

```
helm install substrate oci://ghcr.io/rashadism/substrate/helm/substrate \
  --version 0.2.0 -n ate-system --create-namespace --set image.tag=v0.2.0
```

### 3. This operator

```
helm install oc-substrate ./helm -n openchoreo-substrate --create-namespace \
  --set storageLocation=gs://<your-snapshot-bucket>/oc-substrate
```

This also installs the `proxy/substrate-actor` `ClusterComponentType` the sample below uses. No separate step for it.

### 4. A sample component

```yaml
apiVersion: openchoreo.dev/v1alpha1
kind: Component
metadata:
  name: counter
  namespace: default
spec:
  autoDeploy: true
  componentType:
    kind: ClusterComponentType
    name: proxy/substrate-actor
  owner:
    projectName: default
---
apiVersion: openchoreo.dev/v1alpha1
kind: Workload
metadata:
  name: counter
  namespace: default
spec:
  container:
    image: ghcr.io/rashadism/substrate/demo-counter:v0.2.0
  endpoints:
    http:
      port: 80
      type: HTTP
      visibility:
      - external
  owner:
    componentName: counter
    projectName: default
```

Apply it, wait for the release to expose an endpoint, then call it:

```
kubectl apply -f counter.yaml

HOST=$(kubectl get releasebinding counter-development -n default \
  -o jsonpath='{.status.endpoints[0].externalURLs.http.host}')
PATH_PREFIX=$(kubectl get releasebinding counter-development -n default \
  -o jsonpath='{.status.endpoints[0].externalURLs.http.path}')

curl -sk "https://$HOST$PATH_PREFIX/"
```

A real response (`hello from: <ip> | ...`) confirms the whole path works: the
actor woke up, served the request, and routed back. `kubectl get actor -A`
showing `Ready` only means the actor object exists; it doesn't mean a
request can actually reach it, so treat a real response as the source of
truth, not just actor readiness. State `SUSPENDED` before the first request
is normal: an actor warm and waiting, not idle.

### 5. See the multiplexing

The default install gives 3 workers at 1 CPU/1Gi each, and an actor defaults
to 500m/512Mi, so 2 actors fit per worker: 6 actors is full capacity.
Deploying more components than that is the actual demo, since it shows what
happens at the ceiling, not just that things fit.

Deploy 10:

```
for i in $(seq 1 10); do
  sed "s/: counter/: counter-$i/g" counter.yaml | kubectl apply -f -
done
```

Call all 10:

```
for i in $(seq 1 10); do
  HOST=$(kubectl get releasebinding counter-$i-development -n default \
    -o jsonpath='{.status.endpoints[0].externalURLs.http.host}')
  PATH_PREFIX=$(kubectl get releasebinding counter-$i-development -n default \
    -o jsonpath='{.status.endpoints[0].externalURLs.http.path}')
  curl -sk "https://$HOST$PATH_PREFIX/" -o /dev/null -w "counter-$i: %{http_code}\n"
done
```

The first 6 to get traffic return `200`; the rest return `503` ("no free
workers available"), because capacity is full. `kubectl ate get workers`
shows all 3 workers at `2/2`.

Now free a slot and watch a previously-denied one take it:

```
kubectl patch releasebinding counter-1-development -n default --type merge \
  -p '{"spec":{"componentTypeEnvironmentConfigs":{"resources":{"cpu":"500m","memory":"512Mi"},"paused":true}}}'

# wait a few seconds for it to actually suspend, then:
curl -sk "https://$HOST$PATH_PREFIX/"   # HOST/PATH_PREFIX for counter-7, or any that got 503
```

That last call now succeeds: `counter-1` gave its slot back, and `counter-7`
woke into it. Nothing frees a slot on its own, this only happened because
something explicitly asked `counter-1` to suspend. See `ARCHITECTURE.md` for
why.
