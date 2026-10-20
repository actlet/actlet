//go:build k8stest

package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session/sessionpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const (
	clusterName = "actlet-e2e"
	// kubeContext is the only context this test ever uses. It lives in a
	// kubeconfig file of its own: the user's kubeconfig is never written.
	kubeContext = "kind-" + clusterName
	namespace   = "actlet-e2e"
	token       = "e2e-token"
	// recordings is a directory on the kind node shared by both gateway
	// replicas.
	recordings = "/var/lib/actlet-e2e-recordings"
)

// kube runs kubectl and kind against the test cluster only.
type kube struct {
	t          *testing.T
	kubeconfig string
}

func (k *kube) cmd(name string, args ...string) *exec.Cmd {
	if name == "kubectl" {
		args = append([]string{"--kubeconfig", k.kubeconfig, "--context", kubeContext, "--namespace", namespace}, args...)
	}
	c := exec.Command(name, args...)
	// Belt and braces: even a tool that ignores the flags sees only the
	// test's kubeconfig.
	c.Env = append(os.Environ(), "KUBECONFIG="+k.kubeconfig)
	return c
}

func (k *kube) run(name string, args ...string) (string, error) {
	var out bytes.Buffer
	c := k.cmd(name, args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

func (k *kube) must(name string, args ...string) string {
	k.t.Helper()
	out, err := k.run(name, args...)
	if err != nil {
		k.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func (k *kube) kubectl(args ...string) string {
	k.t.Helper()
	return k.must("kubectl", args...)
}

func (k *kube) apply(manifest string) {
	k.t.Helper()
	c := k.cmd("kubectl", "apply", "-f", "-")
	c.Stdin = strings.NewReader(manifest)
	if out, err := c.CombinedOutput(); err != nil {
		k.t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
}

// admin sends a command line to a gateway replica's standard input.
func (k *kube) admin(pod, line string) {
	k.t.Helper()
	c := k.cmd("kubectl", "attach", "-i", pod, "-c", "actlet-devgw")
	c.Stdin = strings.NewReader(line + "\n")
	done := make(chan error, 1)
	go func() {
		out, err := c.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			k.t.Fatalf("admin %q on %s: %v", line, pod, err)
		}
	case <-time.After(30 * time.Second):
		_ = c.Process.Kill()
		k.t.Fatalf("admin %q on %s: kubectl attach hung", line, pod)
	}
}

func (k *kube) logs(pod string) string {
	out, _ := k.run("kubectl", "logs", pod)
	return out
}

// waitFor polls cond until it is true or the timeout passes.
func (k *kube) waitFor(what string, timeout time.Duration, cond func() bool) {
	k.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			k.t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

func (k *kube) gatewayPods() []string {
	out, _ := k.run("kubectl", "get", "pods", "-l", "app=actlet-devgw",
		"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}")
	return strings.Fields(out)
}

// connectedReplica returns the gateway pod whose log shows the worker's most
// recent connection after since.
func (k *kube) connectedReplica(since time.Time, exclude string) string {
	var best string
	var bestAt time.Time
	for _, pod := range k.gatewayPods() {
		if pod == exclude {
			continue
		}
		for _, line := range strings.Split(k.logs(pod), "\n") {
			if !strings.Contains(line, `msg="worker connected"`) {
				continue
			}
			at := logTime(line)
			if at.After(since) && at.After(bestAt) {
				best, bestAt = pod, at
			}
		}
	}
	return best
}

var timeField = regexp.MustCompile(`time=(\S+)`)

func logTime(line string) time.Time {
	m := timeField.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	at, _ := time.Parse(time.RFC3339Nano, m[1])
	return at
}

func (k *kube) exitCode(pod string) (int, bool) {
	out, _ := k.run("kubectl", "get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].state.terminated.exitCode}")
	code, err := strconv.Atoi(strings.TrimSpace(out))
	return code, err == nil
}

func currentContext(t *testing.T) string {
	// The user's own kubeconfig, read only.
	out, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		return "(none: " + strings.TrimSpace(string(out)) + ")"
	}
	return strings.TrimSpace(string(out))
}

func TestKubernetesPath(t *testing.T) {
	for _, tool := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not available")
	}
	workerImage := envOr("ACTLET_IMAGE", "actlet:dev")
	gatewayImage := envOr("ACTLET_DEVGW_IMAGE", "actlet-devgw:dev")
	soak := 3 * time.Minute
	if v := os.Getenv("ACTLET_K8S_SOAK"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("ACTLET_K8S_SOAK: %v", err)
		}
		soak = d
	}

	before := currentContext(t)
	t.Logf("kubectl config current-context before: %s", before)
	t.Cleanup(func() {
		after := currentContext(t)
		t.Logf("kubectl config current-context after:  %s", after)
		if after != before {
			t.Errorf("current-context changed from %q to %q", before, after)
		}
	})

	k := &kube{t: t, kubeconfig: filepath.Join(t.TempDir(), "kubeconfig")}
	if out, _ := k.run("kind", "get", "clusters"); strings.Contains(out, clusterName) {
		t.Fatalf("a kind cluster named %s already exists; delete it first: kind delete cluster --name %s", clusterName, clusterName)
	}
	t.Log("creating the kind cluster")
	k.must("kind", "create", "cluster", "--name", clusterName, "--kubeconfig", k.kubeconfig, "--wait", "180s")
	t.Cleanup(func() {
		if os.Getenv("ACTLET_K8S_KEEP") != "" {
			t.Logf("keeping the cluster: kind delete cluster --name %s", clusterName)
			return
		}
		t.Log("deleting the kind cluster")
		if out, err := k.run("kind", "delete", "cluster", "--name", clusterName, "--kubeconfig", k.kubeconfig); err != nil {
			t.Errorf("deleting the cluster: %v\n%s", err, out)
		}
	})
	k.must("kind", "load", "docker-image", workerImage, gatewayImage, "--name", clusterName)

	k.must("kubectl", "create", "namespace", namespace)
	k.apply(gatewayManifest(gatewayImage))
	k.apply(sessionPVC)
	k.kubectl("rollout", "status", "deployment/actlet-devgw", "--timeout", "180s")

	start := time.Now()
	k.apply(workerPod("worker-1", workerImage))
	k.waitFor("worker-1 to connect", 3*time.Minute, func() bool { return k.connectedReplica(start, "") != "" })
	replica := k.connectedReplica(start, "")
	t.Logf("worker-1 connected to %s", replica)

	// 1. A long-lived connection stays up: no reconnects, activity flowing.
	t.Logf("soaking for %v", soak)
	soakStart := time.Now()
	for time.Since(soakStart) < soak {
		time.Sleep(min(30*time.Second, soak-time.Since(soakStart)+time.Second))
		if w := k.logs("worker-1"); strings.Contains(w, "stream lost") {
			t.Fatalf("worker reconnected during the soak:\n%s", w)
		}
		t.Logf("  %v: %d activity events at %s", time.Since(soakStart).Round(time.Second), strings.Count(k.logs(replica), "] "), replica)
	}

	// 2. A slow activity consumer doesn't hold up commands.
	k.admin(replica, "store-delay 1s")
	time.Sleep(3 * time.Second) // let activity back up
	k.admin(replica, "steer slow consumer check")
	var took time.Duration
	k.waitFor("the steer acknowledgement", time.Minute, func() bool {
		took = ackTime(k.logs(replica), "steer")
		return took > 0
	})
	t.Logf("steer acknowledged in %v while activity storage took 1s per batch", took)
	if took >= time.Second {
		t.Errorf("a slow activity consumer delayed a command by %v", took)
	}
	k.admin(replica, "store-delay 0")

	// 3. The replica the worker is connected to goes away: it drains, the
	// worker reconnects to another replica and carries on.
	k.admin(replica, "steer before failover")
	k.waitFor("the steer note", time.Minute, func() bool { return strings.Count(k.logs(replica), "steer: ") >= 2 })
	t.Logf("deleting %s", replica)
	failover := time.Now()
	k.kubectl("delete", "pod", replica, "--wait=false")
	k.waitFor("the worker to reconnect elsewhere", 2*time.Minute, func() bool { return k.connectedReplica(failover, replica) != "" })
	newReplica := k.connectedReplica(failover, replica)
	t.Logf("worker-1 reconnected to %s after %v", newReplica, time.Since(failover).Round(time.Millisecond))
	k.admin(newReplica, "steer after failover")
	k.waitFor("the steer note", time.Minute, func() bool { return strings.Contains(k.logs(newReplica), "steer: after failover") })

	// 4. Pause: the pod exits with code 3, and a new pod on the same volume
	// resumes the session.
	k.admin(newReplica, "pause drain")
	k.waitFor("worker-1 to exit", 2*time.Minute, func() bool { _, ok := k.exitCode("worker-1"); return ok })
	if code, _ := k.exitCode("worker-1"); code != 3 {
		t.Fatalf("worker-1 exited with %d, want 3 (paused)\n%s", code, k.logs("worker-1"))
	}
	t.Log("worker-1 paused: exit code 3")

	resume := time.Now()
	k.apply(workerPod("worker-2", workerImage))
	k.waitFor("worker-2 to connect", 3*time.Minute, func() bool { return k.connectedReplica(resume, "") != "" })
	resumed := k.connectedReplica(resume, "")
	if w := k.logs("worker-2"); !strings.Contains(w, "epoch=2") {
		t.Fatalf("worker-2 didn't resume the session as epoch 2:\n%s", w)
	}
	t.Logf("worker-2 resumed the session (epoch 2) on %s", resumed)
	k.waitFor("worker-2 to start working", time.Minute, func() bool { return strings.Contains(k.logs(resumed), "started (epoch 2, resumed true)") })
	k.admin(resumed, "stop kill")
	k.waitFor("worker-2 to exit", 2*time.Minute, func() bool { _, ok := k.exitCode("worker-2"); return ok })
	if code, _ := k.exitCode("worker-2"); code != 4 {
		t.Fatalf("worker-2 exited with %d, want 4 (stopped)\n%s", code, k.logs("worker-2"))
	}
	t.Log("worker-2 stopped: exit code 4")

	// Everything the gateways recorded, across both replicas and both runs:
	// no duplicates, no gaps.
	node := clusterName + "-control-plane"
	recDir := t.TempDir()
	k.must("docker", "cp", node+":"+recordings+"/.", recDir)
	recs, _ := filepath.Glob(filepath.Join(recDir, "*.actrec"))
	if len(recs) != 1 {
		t.Fatalf("found %d recordings, want 1", len(recs))
	}
	events := readEvents(t, recs[0])
	perEpoch := checkSequence(t, events)
	steers := map[string]int{}
	for _, ev := range events {
		if s := ev.GetSteer(); s != nil {
			steers[s.GetNote()]++
		}
	}
	t.Logf("recording: %d events, %v per epoch, steer notes %v", len(events), perEpoch, steers)
	for _, note := range []string{"slow consumer check", "before failover", "after failover"} {
		if steers[note] != 1 {
			t.Errorf("steer note %q recorded %d times, want once", note, steers[note])
		}
	}
	if len(perEpoch) != 2 {
		t.Errorf("recording has %d epochs, want 2", len(perEpoch))
	}

	// The worker's journal on the volume holds each command once.
	out := k.must("docker", "exec", node, "find", "/var/local-path-provisioner", "-name", "session.actlet")
	paths := strings.Fields(out)
	if len(paths) != 1 {
		t.Fatalf("found session journals %v, want one", paths)
	}
	jpath := filepath.Join(t.TempDir(), "session.actlet")
	k.must("docker", "cp", node+":"+paths[0], jpath)
	commands := readCommands(t, jpath)
	t.Logf("journal: %d commands: %v", len(commands), commands)
	seen := map[string]bool{}
	for _, id := range commands {
		if seen[id] {
			t.Errorf("command %s journaled twice", id)
		}
		seen[id] = true
	}
	if len(commands) != 5 { // 3 steer notes, pause, stop
		t.Errorf("journal holds %d commands, want 5", len(commands))
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

var ackPattern = regexp.MustCompile(`(\w+): accepted.*\(in ([0-9.]+[a-zµ]+)\)`)

// ackTime returns how long the last acknowledged command of the given kind
// took, from a gateway's log.
func ackTime(log, what string) time.Duration {
	var took time.Duration
	for _, line := range strings.Split(log, "\n") {
		m := ackPattern.FindStringSubmatch(line)
		if m == nil || m[1] != what {
			continue
		}
		if d, err := time.ParseDuration(m[2]); err == nil {
			took = d
		}
	}
	return took
}

func readEvents(t *testing.T, path string) []*gatewayv0.ActivityEvent {
	t.Helper()
	var out []*gatewayv0.ActivityEvent
	eachRecord(t, path, func(r journal.Record) {
		ev := new(gatewayv0.ActivityEvent)
		if err := proto.Unmarshal(r.Data, ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	})
	return out
}

func readCommands(t *testing.T, path string) []string {
	t.Helper()
	var ids []string
	eachRecord(t, path, func(r journal.Record) {
		if r.Kind != "command" {
			return
		}
		var c sessionpb.Command
		if err := proto.Unmarshal(r.Data, &c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.GetId())
	})
	return ids
}

func eachRecord(t *testing.T, path string, fn func(journal.Record)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range b.Records {
			fn(rec)
		}
	}
}

// checkSequence fails the test unless every epoch's events are numbered
// 1, 2, 3... with no duplicates, and returns the event count per epoch.
func checkSequence(t *testing.T, evs []*gatewayv0.ActivityEvent) map[uint64]uint64 {
	t.Helper()
	next := map[uint64]uint64{}
	seen := map[string]bool{}
	for _, ev := range evs {
		if seen[ev.GetId()] {
			t.Fatalf("event %s recorded twice", ev.GetId())
		}
		seen[ev.GetId()] = true
		if want := next[ev.GetEpoch()] + 1; ev.GetSeq() != want {
			t.Fatalf("epoch %d: event %d follows %d", ev.GetEpoch(), ev.GetSeq(), want-1)
		}
		next[ev.GetEpoch()] = ev.GetSeq()
	}
	return next
}

func gatewayManifest(image string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: actlet-devgw
  namespace: %[1]s
spec:
  selector:
    app: actlet-devgw
  ports:
    - name: grpc
      port: 7400
      targetPort: 7400
      appProtocol: grpc
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: actlet-devgw
  namespace: %[1]s
spec:
  replicas: 2
  selector:
    matchLabels:
      app: actlet-devgw
  template:
    metadata:
      labels:
        app: actlet-devgw
    spec:
      terminationGracePeriodSeconds: 20
      containers:
        - name: actlet-devgw
          image: %[2]s
          imagePullPolicy: IfNotPresent
          args: ["-token", "%[3]s", "-drain-timeout", "10s", "-demo", "{\"steps\":10000,\"step\":\"2s\",\"ticks\":2}"]
          # Admin commands arrive with kubectl attach.
          stdin: true
          ports:
            - containerPort: 7400
          readinessProbe:
            tcpSocket:
              port: 7400
            periodSeconds: 2
          # Both replicas share the node directory holding the recordings;
          # root so the image's non-root user needn't own it.
          securityContext:
            runAsUser: 0
          volumeMounts:
            - name: recordings
              mountPath: /var/lib/actlet-devgw
      volumes:
        - name: recordings
          hostPath:
            path: %[4]s
            type: DirectoryOrCreate
`, namespace, image, token, recordings)
}

const sessionPVC = `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: actlet-session
  namespace: ` + namespace + `
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 100Mi
`

func workerPod(name, image string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 20
  containers:
    - name: actlet
      image: %[3]s
      imagePullPolicy: IfNotPresent
      args: ["run"]
      env:
        - name: ACTLET_GATEWAY
          value: actlet-devgw:7400
        - name: ACTLET_TOKEN
          value: %[4]s
        - name: ACTLET_SESSION
          value: /var/lib/actlet
      volumeMounts:
        - name: session
          mountPath: /var/lib/actlet
  volumes:
    - name: session
      persistentVolumeClaim:
        claimName: actlet-session
`, name, namespace, image, token)
}
