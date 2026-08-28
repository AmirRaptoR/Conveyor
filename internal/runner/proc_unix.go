//go:build unix

package runner

import (
	"bytes"
	"io"
	"math/rand"
	"os/exec"
	"strconv"
	"syscall"
)

// setPgid puts the child in its own process group so a timeout can kill the
// whole tree. An AI stage script spawns subagents; killing only the parent
// leaves them running and holding the source's lock.
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the whole process group. The caller escalates TERM to KILL.
func killGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func randSuffix() string { return strconv.FormatInt(int64(rand.Int31n(1<<24)), 36) }
