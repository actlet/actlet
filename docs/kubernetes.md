# Running on Kubernetes

Actlet workers connect out to the orchestrator's gateway over gRPC (HTTP/2). This document records what we tested about that connection inside Kubernetes, and what we recommend to whoever deploys a gateway and workers there. `make test-k8s` reproduces the test in a throwaway [kind](https://kind.sigs.k8s.io/) cluster.

## What the test runs

`make test-k8s` builds both images and then:

1. Creates a kind cluster named `actlet-e2e`, with a kubeconfig file of its own. Your kubeconfig and its current context are never touched, and the test checks that.
2. Deploys `actlet-devgw` as a **2-replica Deployment behind a ClusterIP Service**. The replicas share one directory on the node for their activity recordings, so either replica can continue a session's recording.
3. Starts a worker Pod (`actlet run`), with its session folder on a **PersistentVolumeClaim** and the Service as its gateway address.
4. Runs four scenarios:
   - **Long-lived streams.** The control and activity streams stay up for three minutes (`ACTLET_K8S_SOAK` changes this). The test checks that the worker never reconnects and that activity keeps flowing.
   - **Slow consumer.** The connected replica stores activity one second per batch while a command is sent. The command's acknowledgement must arrive in under a second.
   - **Failover.** The Pod of the replica the worker is connected to is deleted. The replica drains, and the worker reconnects to the other one, registers again and carries on.
   - **Pause and resume.** A pause makes the worker Pod exit with code 3. A new Pod on the same volume resumes the session as epoch 2, and is then stopped (exit code 4).
5. Checks the outcome. The shared recording holds every event of both runs exactly once and in sequence, each operator note appears once, and the worker's journal holds each command once.
6. Deletes the cluster, even when the test fails (`ACTLET_K8S_KEEP=1` keeps it for debugging).

## Findings

### Long-lived HTTP/2 through a ClusterIP Service

A ClusterIP Service balances **connections**, not requests. kube-proxy picks a backend Pod when a TCP connection opens, and every gRPC stream on that connection goes to the same Pod for as long as the connection lives. A worker's control and activity streams therefore stay pinned to one gateway replica.

That is what Actlet wants: a session's streams go to one replica. It also means:

- Load is spread only as workers connect. Adding replicas doesn't move existing workers.
- Moving workers off a replica takes an explicit step: draining.

### Draining and failover

A gateway replica that stops must drain:

- `sdk.Server.Drain` first sends HTTP/2 GOAWAY, so no new streams start on existing connections.
- It then ends each control stream, and each activity stream once its in-flight batch is stored and acknowledged, with `UNAVAILABLE`.
- Workers reconnect on a **new** TCP connection, which kube-proxy routes to a replica that is still ready.
- The new replica doesn't know the session, so the worker registers again with the same session ID and epoch. Activity resumes from what the shared store holds.

With the dev gateway, the worker was working against the other replica within a second or two of the Pod's deletion.

For this to work:

- Give the gateway Pod a `terminationGracePeriodSeconds` longer than its drain timeout. The dev gateway drains within `-drain-timeout`, 10s by default.
- Use a **readiness probe**, so a Pod that is starting isn't chosen yet and a terminating one drops out of the Service's endpoints.
- Replicas must **share activity state**. A replica that takes over a session must know what the previous one stored, or deduplication fails. The dev gateway shares a directory of recordings, and each session's recording is locked by the replica serving it and released when that replica drains. That only works on one node; a real orchestrator needs a shared database or object store.
- Commands that a draining replica hadn't had acknowledged must be resent by the orchestrator. Workers recognise repeated command IDs, so resending is safe.

### Keepalives

| Side | Setting |
|---|---|
| Worker (client) | ping every 20s when idle, 10s timeout, also without active streams |
| Gateway (`sdk.ServerOptions()`) | allow client pings every 5s or more; ping idle clients every 30s, 10s timeout |
| Control stream | the worker reports its status every 10s |

The connection stayed up for the whole soak with no reconnects. Keepalive pings also keep conntrack and NAT entries fresh, and let a dead peer be found within about 30s.

A gateway must enforce a keepalive minimum no stricter than the worker's 20s ping, or it closes the connection with `ENHANCE_YOUR_CALM`. `sdk.ServerOptions()` does that for you.

### Recommendations for deployers (not tested)

**A service mesh** (Istio, Linkerd) proxies HTTP/2 at L7 and balances per stream rather than per connection, which is fine for Actlet. Watch:

- Idle and stream timeouts. Envoy's default stream idle timeout is 5 minutes, and an activity stream can be quiet for longer while a task waits. The worker reconnects when a stream is cut, so this costs only a reconnect; raise the timeout to avoid the churn.
- Draining. The sidecar must outlive the gateway's drain, for example with a `holdApplicationUntilProxyStarts`-style setting and a drain duration longer than the gateway's.

**An ingress or L7 load balancer** in front of the gateway, for workers outside the cluster:

- It must support gRPC/HTTP/2 end to end.
- Its read timeouts must allow long-lived streams. NGINX ingress, for example, defaults to `grpc_read_timeout 60s`: the control stream can be silent from the gateway's side for longer than that, and would be cut every minute. Set read and send timeouts to an hour or more.
- Terminate TLS at the ingress, or pass it through, and run workers with `-tls`.

**Workers:**

- Keep the session folder on a PersistentVolumeClaim so a paused session can resume in a new Pod.
- Set `terminationGracePeriodSeconds` above the worker's `-signal-deadline` (default 8s), so a Pod deletion pauses cleanly (exit code 3).
- Use `restartPolicy: Never` or a Job, and let the orchestrator decide when to resume.

## Running the test

```sh
make test-k8s                 # needs Docker and kind; skips without them
ACTLET_K8S_SOAK=30s make test-k8s   # a shorter soak
```

The test never uses any kubeconfig context except `kind-actlet-e2e`, which it keeps in a temporary kubeconfig file. It refuses to run if a kind cluster named `actlet-e2e` already exists.
