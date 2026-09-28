// Package ci reads the GitLab CI predefined variables that identify the merge request.
package ci

import (
	"errors"
	"os"
	"strconv"
)

type MRContext struct {
	ServerURL string
	// Project is a numeric ID or a namespace/path.
	Project string
	MRIID   int64
	// HeadSHA is the MR source commit the pipeline runs for; empty outside CI.
	HeadSHA string
	RepoDir string
}

// FromEnv reads CI_* variables. Missing values are left empty so flags can fill them.
func FromEnv(getenv func(string) string) MRContext {
	c := MRContext{
		ServerURL: getenv("CI_SERVER_URL"),
		Project:   getenv("CI_MERGE_REQUEST_PROJECT_ID"),
		// In merged-results pipelines CI_COMMIT_SHA is a merge commit; the MR head is this one.
		HeadSHA: getenv("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA"),
		RepoDir: getenv("CI_PROJECT_DIR"),
	}
	if c.Project == "" {
		c.Project = getenv("CI_PROJECT_ID")
	}
	if c.HeadSHA == "" {
		c.HeadSHA = getenv("CI_COMMIT_SHA")
	}
	if n, err := strconv.ParseInt(getenv("CI_MERGE_REQUEST_IID"), 10, 64); err == nil {
		c.MRIID = n
	}
	if c.RepoDir == "" {
		c.RepoDir, _ = os.Getwd()
	}
	return c
}

func (c MRContext) Validate() error {
	var errs []error
	if c.ServerURL == "" {
		errs = append(errs, errors.New("GitLab URL unknown: set gitlab.url, --gitlab-url or run inside GitLab CI"))
	}
	if c.Project == "" {
		errs = append(errs, errors.New("project unknown: pass --project or run in a merge request pipeline"))
	}
	if c.MRIID == 0 {
		errs = append(errs, errors.New("merge request unknown: pass --mr or run in a merge request pipeline (CI_MERGE_REQUEST_IID)"))
	}
	return errors.Join(errs...)
}
