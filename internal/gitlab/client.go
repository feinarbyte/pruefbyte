// Package gitlab is a small wrapper around client-go, bound to one merge request.
// It exposes only what the review needs, in plain types that are easy to fake in tests.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

type MR struct {
	IID          int64
	Title        string
	Description  string
	SourceBranch string
	TargetBranch string
	Author       string
	Draft        bool
	Labels       []string
	WebURL       string
	BaseSHA      string
	StartSHA     string
	HeadSHA      string
}

type FileDiff struct {
	OldPath, NewPath string
	Diff             string
	NewFile          bool
	DeletedFile      bool
	RenamedFile      bool
}

type Position struct {
	BaseSHA, StartSHA, HeadSHA string
	OldPath, NewPath           string
	OldLine, NewLine           int
}

type Note struct {
	ID         int64
	AuthorID   int64
	Body       string
	System     bool
	Resolvable bool
	Resolved   bool
	Position   *Position
}

type Discussion struct {
	ID             string
	IndividualNote bool
	Notes          []Note
}

// API is what the review orchestrator uses. *Client implements it.
type API interface {
	CurrentUserID(ctx context.Context) (int64, error)
	GetMR(ctx context.Context) (*MR, error)
	ListDiffs(ctx context.Context) ([]FileDiff, error)
	ListDiscussions(ctx context.Context) ([]Discussion, error)
	CreateDiscussion(ctx context.Context, body string, pos *Position) error
	CreateNote(ctx context.Context, body string) error
	UpdateNote(ctx context.Context, noteID int64, body string) error
	ReplyAndResolve(ctx context.Context, discussionID, body string) error
	Compare(ctx context.Context, from, to string) ([]FileDiff, error)
}

type Client struct {
	gl      *gl.Client
	project string
	mrIID   int64
}

// New creates a client authenticated with the bot's personal access token.
// client-go retries 429 and 5xx responses with backoff.
func New(baseURL, token, project string, mrIID int64) (*Client, error) {
	c, err := gl.NewClient(token, gl.WithBaseURL(baseURL))
	if err != nil {
		return nil, err
	}
	return &Client{gl: c, project: project, mrIID: mrIID}, nil
}

// IsBadRequest reports whether err is a GitLab 400, which is how GitLab rejects
// a discussion position it cannot resolve against the diff.
func IsBadRequest(err error) bool {
	var er *gl.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusBadRequest
}

func (c *Client) CurrentUserID(ctx context.Context) (int64, error) {
	u, _, err := c.gl.Users.CurrentUser(gl.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("resolving bot user (is the token valid?): %w", err)
	}
	return u.ID, nil
}

func (c *Client) GetMR(ctx context.Context) (*MR, error) {
	m, _, err := c.gl.MergeRequests.GetMergeRequest(c.project, c.mrIID, nil, gl.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetching merge request !%d: %w", c.mrIID, err)
	}
	mr := &MR{
		IID:          m.IID,
		Title:        m.Title,
		Description:  m.Description,
		SourceBranch: m.SourceBranch,
		TargetBranch: m.TargetBranch,
		Draft:        m.Draft,
		Labels:       m.Labels,
		WebURL:       m.WebURL,
		BaseSHA:      m.DiffRefs.BaseSha,
		StartSHA:     m.DiffRefs.StartSha,
		HeadSHA:      m.DiffRefs.HeadSha,
	}
	if m.Author != nil {
		mr.Author = m.Author.Username
	}
	return mr, nil
}

func (c *Client) ListDiffs(ctx context.Context) ([]FileDiff, error) {
	var out []FileDiff
	opt := &gl.ListMergeRequestDiffsOptions{ListOptions: gl.ListOptions{PerPage: 100, Page: 1}}
	for {
		page, resp, err := c.gl.MergeRequests.ListMergeRequestDiffs(c.project, c.mrIID, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("listing merge request diffs: %w", err)
		}
		for _, d := range page {
			out = append(out, FileDiff{OldPath: d.OldPath, NewPath: d.NewPath, Diff: d.Diff,
				NewFile: d.NewFile, DeletedFile: d.DeletedFile, RenamedFile: d.RenamedFile})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) ListDiscussions(ctx context.Context) ([]Discussion, error) {
	var out []Discussion
	opt := &gl.ListMergeRequestDiscussionsOptions{ListOptions: gl.ListOptions{PerPage: 100, Page: 1}}
	for {
		page, resp, err := c.gl.Discussions.ListMergeRequestDiscussions(c.project, c.mrIID, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("listing discussions: %w", err)
		}
		for _, d := range page {
			disc := Discussion{ID: d.ID, IndividualNote: d.IndividualNote}
			for _, n := range d.Notes {
				note := Note{ID: n.ID, AuthorID: n.Author.ID, Body: n.Body, System: n.System,
					Resolvable: n.Resolvable, Resolved: n.Resolved}
				if p := n.Position; p != nil {
					note.Position = &Position{BaseSHA: p.BaseSHA, StartSHA: p.StartSHA, HeadSHA: p.HeadSHA,
						OldPath: p.OldPath, NewPath: p.NewPath, OldLine: int(p.OldLine), NewLine: int(p.NewLine)}
				}
				disc.Notes = append(disc.Notes, note)
			}
			out = append(out, disc)
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) CreateDiscussion(ctx context.Context, body string, pos *Position) error {
	opt := &gl.CreateMergeRequestDiscussionOptions{Body: gl.Ptr(body)}
	if pos != nil {
		po := &gl.PositionOptions{
			PositionType: gl.Ptr("text"),
			BaseSHA:      gl.Ptr(pos.BaseSHA),
			StartSHA:     gl.Ptr(pos.StartSHA),
			HeadSHA:      gl.Ptr(pos.HeadSHA),
			NewPath:      gl.Ptr(pos.NewPath),
			OldPath:      gl.Ptr(pos.OldPath),
		}
		if pos.NewLine > 0 {
			po.NewLine = gl.Ptr(int64(pos.NewLine))
		}
		if pos.OldLine > 0 {
			po.OldLine = gl.Ptr(int64(pos.OldLine))
		}
		opt.Position = po
	}
	_, _, err := c.gl.Discussions.CreateMergeRequestDiscussion(c.project, c.mrIID, opt, gl.WithContext(ctx))
	return err
}

func (c *Client) CreateNote(ctx context.Context, body string) error {
	_, _, err := c.gl.Notes.CreateMergeRequestNote(c.project, c.mrIID,
		&gl.CreateMergeRequestNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx))
	return err
}

func (c *Client) UpdateNote(ctx context.Context, noteID int64, body string) error {
	_, _, err := c.gl.Notes.UpdateMergeRequestNote(c.project, c.mrIID, noteID,
		&gl.UpdateMergeRequestNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx))
	return err
}

func (c *Client) ReplyAndResolve(ctx context.Context, discussionID, body string) error {
	if body != "" {
		if _, _, err := c.gl.Discussions.AddMergeRequestDiscussionNote(c.project, c.mrIID, discussionID,
			&gl.AddMergeRequestDiscussionNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx)); err != nil {
			return err
		}
	}
	_, _, err := c.gl.Discussions.ResolveMergeRequestDiscussion(c.project, c.mrIID, discussionID,
		&gl.ResolveMergeRequestDiscussionOptions{Resolved: gl.Ptr(true)}, gl.WithContext(ctx))
	return err
}

func (c *Client) Compare(ctx context.Context, from, to string) ([]FileDiff, error) {
	cmp, _, err := c.gl.Repositories.Compare(c.project,
		&gl.CompareOptions{From: gl.Ptr(from), To: gl.Ptr(to), Straight: gl.Ptr(true)}, gl.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("comparing %s..%s: %w", from, to, err)
	}
	out := make([]FileDiff, 0, len(cmp.Diffs))
	for _, d := range cmp.Diffs {
		out = append(out, FileDiff{OldPath: d.OldPath, NewPath: d.NewPath, Diff: d.Diff,
			NewFile: d.NewFile, DeletedFile: d.DeletedFile, RenamedFile: d.RenamedFile})
	}
	return out, nil
}
