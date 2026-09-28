package fakeadapter

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// GroupInfo is the group mode's published group.json.
type GroupInfo struct {
	LeaderPID     int `json:"leader_pid"`
	DescendantPID int `json:"descendant_pid"`
}

// groupPoll is the group mode's trigger polling interval.
const groupPoll = 10 * time.Millisecond

// runGroup is the "group" task mode (see EnvTaskGroupDir).
func runGroup(env Env, stdout, stderr io.Writer, getenv func(string) string) int {
	dir := getenv(EnvTaskGroupDir)
	if !filepath.IsAbs(dir) {
		fmt.Fprintf(stderr, "fake-adapter: %s must be an absolute directory\n", EnvTaskGroupDir)
		return 2
	}
	term := getenv(EnvTaskDescendantTerm)
	if term == "" {
		term = TermExit
	}
	if term != TermExit && term != TermIgnore {
		fmt.Fprintf(stderr, "fake-adapter: %s must be exit or ignore\n", EnvTaskDescendantTerm)
		return 2
	}
	full, err := env.withDefaults()
	if err != nil {
		fmt.Fprintf(stderr, "fake-adapter: %v\n", err)
		return 1
	}
	child, err := spawnDescendant(full, Options{GrandchildTermMode: term, Duration: time.Hour})
	if err != nil {
		fmt.Fprintf(stderr, "fake-adapter: spawn: %v\n", err)
		return 1
	}
	// Output before the trigger (a crash-after-output window).
	fmt.Fprintf(stdout, "native started pid=%d\n", full.PID)
	info := GroupInfo{LeaderPID: full.PID, DescendantPID: child.cmd.Process.Pid}
	b, _ := json.Marshal(info)
	tmp := filepath.Join(dir, GroupFile+".tmp-"+strconv.Itoa(full.PID))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil || os.Rename(tmp, filepath.Join(dir, GroupFile)) != nil {
		fmt.Fprintf(stderr, "fake-adapter: group file: %v\n", err)
		child.kill()
		return 1
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, GroupTrigger)); err == nil {
			break
		}
		<-full.After(groupPoll)
	}
	fmt.Fprintf(stdout, "native output pid=%d\n", full.PID)
	io.WriteString(stdout, FinalMarker)
	return 0
}
