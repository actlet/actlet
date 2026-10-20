//go:build dockertest

package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDockerVolumeSurvival runs a journal writer in the actlet container
// image on a named volume, kills the container mid-write, and checks from a
// fresh container that every acknowledged record survived. It then deletes
// the volume to show the boundary: without the volume the session is gone.
//
// Run it with `make test-docker`, which builds the image first. It needs a
// Docker daemon and skips without one.
func TestDockerVolumeSurvival(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not available")
	}
	image := os.Getenv("ACTLET_IMAGE")
	if image == "" {
		image = "actlet:dev"
	}
	if out, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Skipf("image %s not available (run make image): %s", image, bytes.TrimSpace(out))
	}

	// The writer is this package's crash helper, and the reader actlet-cli,
	// both built for the daemon's architecture and mounted into the image.
	arch := strings.TrimSpace(dockerOut(t, "version", "--format", "{{.Server.Arch}}"))
	binDir := t.TempDir()
	for _, build := range []*exec.Cmd{
		exec.Command("go", "test", "-c", "-o", filepath.Join(binDir, "journal.test"), "."),
		exec.Command("go", "build", "-o", filepath.Join(binDir, "actlet-cli"), "../../cmd/actlet-cli"),
	} {
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %v: %v\n%s", build.Args, err, out)
		}
	}

	volume := fmt.Sprintf("actlet-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	dockerOut(t, "volume", "create", volume)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", volume).Run() })
	// cli runs actlet-cli in a container with the volume mounted.
	cli := func(args ...string) []string {
		return append([]string{"run", "--rm", "--entrypoint", "/t/actlet-cli",
			"-v", binDir + ":/t:ro", "-v", volume + ":/var/lib/actlet", image}, args...)
	}

	const dir = "/var/lib/actlet"
	const journalPath = dir + "/session.actlet"
	acked := map[uint64]string{}
	var maxAcked uint64

	for round := range 3 {
		name := fmt.Sprintf("%s-writer-%d", volume, round)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
		dockerOut(t, "run", "-d", "--name", name,
			"--entrypoint", "/t/journal.test",
			"-v", binDir+":/t:ro", "-v", volume+":"+dir,
			"-e", crashHelperEnv+"="+journalPath,
			image, "-test.run=^TestCrashHelper$")

		// Let the writer acknowledge some records, then kill the whole
		// container: SIGKILL, no chance to clean up.
		deadline := time.Now().Add(30 * time.Second)
		for strings.Count(dockerOut(t, "logs", name), "\nack ") < 100 {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: writer didn't get going:\n%s", round, dockerOut(t, "logs", name))
			}
			time.Sleep(50 * time.Millisecond)
		}
		dockerOut(t, "kill", name)
		n := 0
		sc := bufio.NewScanner(strings.NewReader(dockerOut(t, "logs", name)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 3 && f[0] == "ack" {
				seq, _ := strconv.ParseUint(f[1], 10, 64)
				acked[seq] = f[2]
				maxAcked = max(maxAcked, seq)
				n++
			} else if len(f) > 0 && f[0] == "error" {
				t.Fatalf("round %d: writer: %s", round, sc.Text())
			}
		}
		dockerOut(t, "rm", name)

		// A new container sees every acknowledged record on the volume.
		export := dockerOut(t, cli("journal", "export", "--jsonl", journalPath)...)
		var got uint64
		sc = bufio.NewScanner(strings.NewReader(export))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var rec struct {
				Seq  uint64 `json:"seq"`
				Data string `json:"data"`
			}
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("round %d: export line %q: %v", round, sc.Text(), err)
			}
			if rec.Seq != got+1 {
				t.Fatalf("round %d: export jumps from seq %d to %d", round, got, rec.Seq)
			}
			got = rec.Seq
			if want, ok := acked[rec.Seq]; ok && !strings.HasPrefix(rec.Data, want) {
				t.Fatalf("round %d: seq %d holds %.16q, acknowledged %q", round, rec.Seq, rec.Data, want)
			}
		}
		if got < maxAcked {
			t.Fatalf("round %d: recovered up to seq %d, but %d was acknowledged", round, got, maxAcked)
		}
		t.Logf("round %d: %d records acknowledged this round, %d recovered in total (last acknowledged %d)", round, n, got, maxAcked)
	}

	// The boundary: the session lives on the volume, not in the container.
	dockerOut(t, "volume", "rm", volume)
	dockerOut(t, "volume", "create", volume)
	cmd := exec.Command("docker", cli("journal", "inspect", journalPath)...)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no such file") {
		t.Fatalf("after deleting the volume: err=%v, output:\n%s", err, out)
	}
	t.Logf("after deleting the volume: %s", bytes.TrimSpace(out))
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
