//go:build !windows

package ocr

import (
	"context"
	"os/exec"
)

// shellCommand runs command through sh.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}
