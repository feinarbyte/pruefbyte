//go:build windows

package ocr

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs command through cmd.exe. Go's argument escaping (\") means
// nothing to cmd.exe, so a command with quotes inside, such as
// op read "op://vault/item/key", would reach the program mangled. The command
// line is built by hand instead, as Node's shell option does: /s strips only the
// outer quotes and passes the rest as written.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
	return cmd
}
