package gitlab

import (
	"context"
	"fmt"
	"io"
)

// DryRun passes reads through to the wrapped API and prints writes instead of sending them.
type DryRun struct {
	API
	Out io.Writer
}

func (d DryRun) CreateDiscussion(_ context.Context, body string, pos *Position) error {
	loc := "(no position)"
	if pos != nil {
		loc = fmt.Sprintf("%s new_line=%d old_line=%d", pos.NewPath, pos.NewLine, pos.OldLine)
	}
	fmt.Fprintf(d.Out, "--- [dry-run] discussion on %s\n%s\n\n", loc, body)
	return nil
}

func (d DryRun) CreateNote(_ context.Context, body string) error {
	fmt.Fprintf(d.Out, "--- [dry-run] note\n%s\n\n", body)
	return nil
}

func (d DryRun) UpdateNote(_ context.Context, id int64, body string) error {
	fmt.Fprintf(d.Out, "--- [dry-run] update note %d\n%s\n\n", id, body)
	return nil
}

func (d DryRun) ReplyAndResolve(_ context.Context, id, body string) error {
	fmt.Fprintf(d.Out, "--- [dry-run] resolve discussion %s: %s\n\n", id, body)
	return nil
}
