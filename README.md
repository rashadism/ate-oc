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
