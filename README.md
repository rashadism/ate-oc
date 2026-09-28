# oc-substrate

Run OpenChoreo components on [Agent Substrate](https://github.com/agent-substrate/substrate): snapshot-backed actors multiplexed onto shared warm workers, using only OpenChoreo's existing abstractions.

## Install

Install in this order: OpenChoreo, then Agent Substrate, then this operator, then a sample component.

### 1. OpenChoreo

A running OpenChoreo control plane and data plane are a prerequisite. Follow OpenChoreo's own [quick start guide](https://openchoreo.dev/docs/getting-started/quick-start-guide).

### 2. Agent Substrate

```
helm install substrate oci://ghcr.io/rashadism/substrate/helm/substrate \
  --version 0.2.0 -n ate-system --create-namespace --set image.tag=v0.2.0
```

A cold install can take a couple of minutes to fully settle. If `ate-controller` or `atelet` reject valid peers with `certificate signed by unknown authority` and it hasn't cleared after a couple of minutes, restart them once:

```
kubectl rollout restart deploy/ate-controller -n ate-system
kubectl rollout restart daemonset/atelet -n ate-system
```

If worker capacity still reports `0` after that (`kubectl ate get workers`), restart the worker pods too:

```
kubectl delete pods -n ate-workers -l ate.dev/worker-pool=default
```

### 3. This operator

No published image yet. Build and push one first:

```
make docker-build IMG=<your-registry>/oc-substrate:<tag>
docker push <your-registry>/oc-substrate:<tag>
```

Then install:

```
helm install oc-substrate ./helm -n openchoreo-substrate --create-namespace \
  --set image.repository=<your-registry>/oc-substrate \
  --set image.tag=<tag> \
  --set storageLocation=gs://<your-snapshot-bucket>/oc-substrate
```

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
    name: proxy/substrate-service
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
      port: 8080
      type: HTTP
      visibility:
      - external
  owner:
    componentName: counter
    projectName: default
```

Apply it, then check the underlying actor comes up:

```
kubectl apply -f counter.yaml
kubectl get actor -A
```

It's ready once the actor's `Ready` condition is `True` (state `SUSPENDED` is normal: an actor warm and waiting for its first request, not idle).
