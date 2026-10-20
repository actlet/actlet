//go:build ignore

// process measures the built binaries as processes: their sizes, how long
// actlet run takes from starting to being registered with a gateway, and
// its resident memory while a session idles. It needs make build; make
// bench runs it.
//
//	go run test/bench/process.go [-bin dir] [-runs n]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	bin := flag.String("bin", "bin", "the directory of the built binaries")
	runs := flag.Int("runs", 5, "runs of actlet run to measure")
	flag.Parse()
	fmt.Printf("## %s/%s, %d CPUs\n\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Println("| Binary | Size |")
	fmt.Println("|---|---|")
	for _, b := range []string{"actlet", "actlet-cli", "actlet-devgw"} {
		fi, err := os.Stat(filepath.Join(*bin, b))
		if err != nil {
			fail(err)
		}
		fmt.Printf("| %s | %.1f MiB |\n", b, float64(fi.Size())/(1<<20))
	}

	tmp, err := os.MkdirTemp("", "actlet-bench-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)
	const addr = "127.0.0.1:7496"
	gw := exec.Command(filepath.Join(*bin, "actlet-devgw"), "-listen", addr, "-token", "bench", "-data", filepath.Join(tmp, "gw"),
		"-demo", `{"steps":1000,"step":"30s"}`)
	if err := gw.Start(); err != nil {
		fail(err)
	}
	defer func() { _ = gw.Process.Kill(); _ = gw.Wait() }()
	time.Sleep(500 * time.Millisecond)

	var starts []time.Duration
	var rss []int
	for i := range *runs {
		session := filepath.Join(tmp, fmt.Sprint("s", i))
		cmd := exec.Command(filepath.Join(*bin, "actlet"), "run", "-session", session, "-no-attach")
		cmd.Env = append(os.Environ(), "ACTLET_GATEWAY="+addr, "ACTLET_TOKEN=bench", "ACTLET_TOOLS_MANIFEST=", "ACTLET_SKILLS=")
		stderr, err := cmd.StderrPipe()
		if err != nil {
			fail(err)
		}
		start := time.Now()
		if err := cmd.Start(); err != nil {
			fail(err)
		}
		registered := make(chan time.Duration, 1)
		go func() {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				if strings.Contains(sc.Text(), "msg=registered") {
					registered <- time.Since(start)
				}
			}
		}()
		select {
		case d := <-registered:
			starts = append(starts, d)
		case <-time.After(10 * time.Second):
			fail(fmt.Errorf("run %d never registered", i))
		}
		time.Sleep(2 * time.Second) // the demo task idles between steps
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(cmd.Process.Pid)).Output()
		if err != nil {
			fail(err)
		}
		kib, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		rss = append(rss, kib)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}
	slices.Sort(starts)
	slices.Sort(rss)
	fmt.Println()
	fmt.Printf("| actlet run, %d runs | Median | Min | Max |\n", *runs)
	fmt.Println("|---|---|---|---|")
	fmt.Printf("| start to registered with the gateway | %v | %v | %v |\n",
		starts[len(starts)/2].Round(100*time.Microsecond), starts[0].Round(100*time.Microsecond), starts[len(starts)-1].Round(100*time.Microsecond))
	fmt.Printf("| resident memory, idle | %.1f MiB | %.1f MiB | %.1f MiB |\n",
		float64(rss[len(rss)/2])/1024, float64(rss[0])/1024, float64(rss[len(rss)-1])/1024)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "process:", err)
	os.Exit(1)
}
