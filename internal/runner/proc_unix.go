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
// leaves them running and holding the source's lock. With Pgid left at zero,
// the child becomes the leader of its own new group, so its own PID doubles
// as the group ID for the rest of the run — no need to ask the kernel again
// once the leader itself may already have been reaped.
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the whole process group by the pgid captured when the
// leader started. ESRCH — the group is already gone — is success, not an
// error worth reporting.
func killGroup(pgid int, sig syscall.Signal) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

// groupAlive reports whether any process still belongs to the group. Once the
// leader is reaped, asking the kernel "what is this pid's group" no longer
// works — the pid is gone — but the group id survives as long as any member
// does, and signal 0 only checks for existence.
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	return syscall.Kill(-pgid, 0) == nil
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func randSuffix() string { return strconv.FormatInt(int64(rand.Int31n(1<<24)), 36) }
