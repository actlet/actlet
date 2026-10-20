package runner

import "time"

// cpuTime isn't measured on Windows.
func cpuTime() time.Duration { return 0 }
