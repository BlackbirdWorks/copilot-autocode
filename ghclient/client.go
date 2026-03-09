// Package ghclient wraps go-github with the operations needed by the poller.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/BlackbirdWorks/copilot-autocode/config"
)

const (
	// RefinementCommentMarker is an invisible HTML comment embedded in every
	// refinement prompt.  The orchestrator counts PR reviews containing this
	// marker to determine how many refinement rounds have already been sent,
	// which means the count survives process restarts.
	RefinementCommentMarker = "<!-- copilot-autocode:refinement -->"

	// MergeConflictCommentMarker is an invisible HTML comment embedded in
	// every merge-conflict @copilot prompt.  Counting these comments on a PR
	// tells the orchestrator how many @copilot attempts have been made so far,
	// which means the count survives process restarts.
	MergeConflictCommentMarker = "<!-- copilot-autocode:merge-conflict -->"

	// CopilotNudgeCommentMarker is an invisible HTML comment embedded in every
	// nudge comment posted when the Copilot coding agent has not started
	// within the configured timeout.  Counting these comments tells the
	// orchestrator how many re-trigger attempts have been made for the current
	// coding cycle so that it can enforce CopilotInvokeMaxRetries.
	CopilotNudgeCommentMarker = "<!-- copilot-autocode:nudge -->"

	// AgentContinueCommentMarker is an invisible HTML comment embedded in
	// every "@copilot continue" comment posted when the agent's workflow run
	// times out during coding or refinement.  Counting these comments tells
	// the orchestrator how many continue attempts have been made so far.
	AgentContinueCommentMarker = "<!-- copilot-autocode:agent-continue -->"

	// MergeConflictContinueCommentMarker is an invisible HTML comment embedded
	// in every "@copilot continue" nudge posted while the agent is stuck
	// resolving merge conflicts.  Keeping this separate from
	// AgentContinueCommentMarker gives the merge-conflict phase its own retry
	// budget so it cannot starve the refinement+CI feedback loop.
	MergeConflictContinueCommentMarker = "<!-- copilot-autocode:merge-conflict-continue -->"

	// LocalResolutionCommentMarker is embedded in the notice posted after a
	// local AI merge resolution attempt.  Counting these comments tells the
	// orchestrator how many local resolution attempts have been made.
	LocalResolutionCommentMarker = "<!-- copilot-autocode:local-resolution -->"

	// PRLinkCommentMarker is embedded in an issue comment to explicitly link it to a PR.
	PRLinkCommentMarker = "<!-- copilot-autocode:pr-link:"

	// IssueLinkCommentMarker is embedded in a PR comment to explicitly link it to an Issue.
	IssueLinkCommentMarker = "<!-- copilot-autocode:issue-link:"

	// CopilotJobIDCommentMarker is embedded in the tracking comment posted
	// after invoking the Copilot API.  It records the job/task ID returned
	// by the API so the orchestrator can avoid duplicate invocations.
	// Format: <!-- copilot-autocode:job-id:UUID -->
	CopilotJobIDCommentMarker = "<!-- copilot-autocode:job-id:"
)

// FailedJobInfo describes a single failed CI job.
type FailedJobInfo struct {
	Name   string // display name of the job
	LogURL string // URL to the raw logs (may be empty if unavailable)
}

// copilotAPIBase is the base URL of the GitHub Copilot API used to create
// agent tasks directly (the same backend that powers the Agents tab).
const copilotAPIBase = "https://api.githubcopilot.com"

// copilotAPIVersion is the API version header sent to the Copilot API.
const copilotAPIVersion = "2026-01-09"

// Pagination page sizes and other numeric constants used throughout the client.
const (
	prStateOpen        = "open"      // GitHub API state value for open issues and PRs
	runStatusCompleted = "completed" // GitHub Actions workflow run status
	perPageDefault     = 100         // default page size for most paginated list calls
	perPageMedium      = 50          // page size for PR and commit list calls
	perPageSmall       = 20          // page size for workflow run calls
	perPageMin         = 10          // page size for small-result-set calls
	maxLogRedirects    = 10          // maximum HTTP redirects when fetching job log URLs
	housPerDay         = 24          // hours in a day, for relative timestamp formatting
	shortSHALen        = 7           // number of chars used for a shortened commit SHA
)

// Client wraps the GitHub SDK client with the settings from Config.
type Client struct {
	gh            *github.Client
	owner         string
	repo          string
	labelQueue    string
	labelCoding   string
	labelReview   string
	labelTakeover string
	mergeMethod   string
	mergeMsg      string
	token         string // PAT used for Copilot API calls
}

// New creates a new Client authenticated with the provided PAT token.
func New(token string, cfg *config.Config) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &Client{
		gh:            github.NewClient(tc),
		owner:         cfg.GitHubOwner,
		repo:          cfg.GitHubRepo,
		labelQueue:    cfg.LabelQueue,
		labelCoding:   cfg.LabelCoding,
		labelReview:   cfg.LabelReview,
		labelTakeover: cfg.LabelTakeover,
		mergeMethod:   cfg.MergeMethod,
		mergeMsg:      cfg.MergeCommitMessage,
		token:         token,
	}
}

// NewTestClient creates a minimal Client suitable for unit tests that need to
// call methods directly (e.g. InvokeAgentAt) without a full config.
func NewTestClient(owner, repo, token string) *Client {
	return &Client{
		gh:    github.NewClient(nil),
		owner: owner,
		repo:  repo,
		token: token,
	}
}

// NewTestClientWithGH creates a Client backed by the provided *github.Client,
// used to point at an httptest server in unit tests.
func NewTestClientWithGH(gh *github.Client, owner, repo string) *Client {
	return &Client{gh: gh, owner: owner, repo: repo}
}

// IssuesByLabel returns all open issues carrying the given label.
func (c *Client) IssuesByLabel(ctx context.Context, label string) ([]*github.Issue, error) {
	var all []*github.Issue
	opts := &github.IssueListByRepoOptions{
		State:       prStateOpen,
		Labels:      []string{label},
		ListOptions: github.ListOptions{PerPage: perPageDefault},
	}
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues (label=%s): %w", label, err)
		}
		// Filter out pull requests (GitHub API returns PRs in issue list too).
		for _, i := range issues {
			if i.PullRequestLinks != nil {
				continue
			}
			// Ignore any issue carrying the manual takeover label.
			takenOver := false
			for _, l := range i.Labels {
				if strings.EqualFold(l.GetName(), c.labelTakeover) {
					takenOver = true
					break
				}
			}
			if takenOver {
				continue
			}
			all = append(all, i)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// AddLabel adds a label to the given issue number.
func (c *Client) AddLabel(ctx context.Context, issueNum int, label string) error {
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, c.owner, c.repo, issueNum, []string{label})
	return err
}

// RemoveLabel removes a label from the given issue number (ignores 404).
func (c *Client) RemoveLabel(ctx context.Context, issueNum int, label string) error {
	_, err := c.gh.Issues.RemoveLabelForIssue(ctx, c.owner, c.repo, issueNum, label)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return nil
}

// SwapLabel atomically transitions an issue from one label to another by
// adding the new label first, then removing the old one.  This ordering
// ensures the issue is never label-less: if AddLabel fails the issue keeps
// the old label (safe); if RemoveLabel fails after AddLabel the issue has
// both labels (recoverable on next tick, and IssuesByLabel de-duplicates
// by issue number).
func (c *Client) SwapLabel(ctx context.Context, issueNum int, oldLabel, newLabel string) error {
	if err := c.AddLabel(ctx, issueNum, newLabel); err != nil {
		return fmt.Errorf("swap label: add %q: %w", newLabel, err)
	}
	if err := c.RemoveLabel(ctx, issueNum, oldLabel); err != nil {
		log.Printf("warning: swap label: added %q but failed to remove %q on issue #%d: %v",
			newLabel, oldLabel, issueNum, err)
	}
	return nil
}

// CloseIssue closes the issue.
func (c *Client) CloseIssue(ctx context.Context, issueNum int) error {
	closed := "closed"
	_, _, err := c.gh.Issues.Edit(ctx, c.owner, c.repo, issueNum, &github.IssueRequest{State: &closed})
	return err
}

func isMatchForIssue(pr *github.PullRequest, bodyRe, titleRe, branchRe *regexp.Regexp) bool {
	if bodyRe.MatchString(pr.GetBody()) || titleRe.MatchString(pr.GetTitle()) {
		return true
	}
	// Copilot Workspace might not link the issue in the body/title yet, but
	// often names branches `copilot/issue-number-description` or similar.
	branch := pr.GetHead().GetRef()
	return branchRe.MatchString(branch)
}

// findLinkedPRFromComments scans an issue's comments for the PRLinkCommentMarker.
// If found, it parses the PR number and returns it.
func (c *Client) findLinkedPRFromComments(ctx context.Context, issueNum int) (int, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return 0, err
		}
		for _, cm := range comments {
			body := cm.GetBody()
			if idx := strings.Index(body, PRLinkCommentMarker); idx != -1 {
				// Parse the number. Format: <!-- copilot-autocode:pr-link:123 -->
				start := idx + len(PRLinkCommentMarker)
				end := strings.Index(body[start:], "-->")
				if end != -1 {
					numStr := strings.TrimSpace(body[start : start+end])
					var prNum int
					if _, err := fmt.Sscanf(numStr, "%d", &prNum); err == nil {
						return prNum, nil
					}
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return 0, nil
}

// OpenPRForIssue finds the first open PR whose title or body references the issue.
func (c *Client) OpenPRForIssue(ctx context.Context, issue *github.Issue) (*github.PullRequest, error) {
	issueNum := issue.GetNumber()

	// 1. Check for explicit comment links on the ISSUE side first.
	// This is our primary source of truth once a link is established.
	// We still call ensureTwoWayLink in case the PR-side comment was missed.
	linkedPRNum, err := c.findLinkedPRFromComments(ctx, issueNum)
	if err != nil {
		log.Printf("issue #%d: step 1 (comment links) error: %v", issueNum, err)
	} else if linkedPRNum != 0 {
		pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, linkedPRNum)
		if err == nil && pr.GetState() == prStateOpen {
			c.ensureTwoWayLink(ctx, issueNum, linkedPRNum)
			return pr, nil
		}
		log.Printf("issue #%d: step 1 found linked PR #%d but it is not open (err=%v)", issueNum, linkedPRNum, err)
	}

	// 2. Search for the explicit link marker on the PR side.
	// This helps discover PRs where the issue-side comment might be missing or delayed.
	markerText := fmt.Sprintf("copilot-autocode:issue-link:%d", issueNum)
	markerQuery := fmt.Sprintf("repo:%s/%s is:pr is:open %s", c.owner, c.repo, markerText)
	markerResult, _, err := c.gh.Search.Issues(ctx, markerQuery, nil)
	if err != nil {
		log.Printf("issue #%d: step 2 (PR-side marker search) error: %v", issueNum, err)
	} else if len(markerResult.Issues) > 0 {
		sr := markerResult.Issues[0]
		pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
		if err == nil && pr.GetState() == prStateOpen {
			c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
			return pr, nil
		}
	}

	// Initial discovery (text-matching heuristics).
	// branchRe matches Copilot branch naming conventions:
	//   copilot/issue-373-description  ("issue" prefix)
	//   copilot/373-description        (bare number)
	//   copilot-swe-agent/issue-373/desc  (slash after number)
	bodyRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	titleRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	branchRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|/)(?:issue-?)?%d(?:[-/]|$)`, issueNum))

	// 3. Native GitHub Search by issue number (body/title/comments).
	query := fmt.Sprintf("repo:%s/%s is:pr is:open %d", c.owner, c.repo, issueNum)
	result, _, err := c.gh.Search.Issues(ctx, query, nil)
	if err != nil {
		log.Printf("issue #%d: step 3 (GitHub search) error: %v", issueNum, err)
	} else {
		if pr := c.findMatchInSearchResults(ctx, issueNum, result.Issues, bodyRe, titleRe, branchRe); pr != nil {
			return pr, nil
		}
		log.Printf("issue #%d: step 3 returned %d search results, none matched", issueNum, len(result.Issues))
	}

	// 4. Fallback: list recent PRs and look for issue number.
	// This handles cases where the search index might be lagging for the very first PR push.
	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State:       prStateOpen,
		ListOptions: github.ListOptions{PerPage: perPageMedium},
	})
	if err != nil {
		log.Printf("issue #%d: step 4 (list PRs) error: %v", issueNum, err)
	} else {
		for _, pr := range prs {
			if isMatchForIssue(pr, bodyRe, titleRe, branchRe) {
				c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
				return pr, nil
			}
		}
		log.Printf("issue #%d: step 4 listed %d open PRs, none matched body/title/branch regex", issueNum, len(prs))
	}

	// 5. Issue timeline: scan for cross-reference events from a PR.
	// This is authoritative for newly created PRs regardless of text matching
	// or search-index lag — GitHub records the reference immediately.
	if pr, err := c.findPRFromTimeline(ctx, issueNum); err == nil && pr != nil {
		c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
		return pr, nil
	} else if err != nil {
		log.Printf("issue #%d: step 5 (timeline) error: %v", issueNum, err)
	}

	log.Printf("issue #%d: OpenPRForIssue: no PR found after all 5 detection steps", issueNum)
	return nil, nil
}

// findPRFromTimeline scans the issue's timeline for cross-reference events
// originating from an open PR. This works immediately when a PR body says
// "Fixes #N", without waiting for search indexing or relying on text matching.
func (c *Client) findPRFromTimeline(ctx context.Context, issueNum int) (*github.PullRequest, error) {
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		events, resp, err := c.gh.Issues.ListIssueTimeline(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return nil, err
		}
		for _, evt := range events {
			if evt.GetEvent() != "cross-referenced" {
				continue
			}
			src := evt.Source
			// GitHub always returns type:"issue" for cross-reference sources,
			// even when the source is a pull request.  Check PullRequestLinks
			// to distinguish PRs from plain issues.
			if src == nil || src.Issue == nil || src.Issue.PullRequestLinks == nil {
				continue
			}
			prNum := src.Issue.GetNumber()
			if prNum == 0 {
				continue
			}
			pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, prNum)
			if err != nil || pr.GetState() != prStateOpen {
				continue
			}
			return pr, nil
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

// findMatchInSearchResults iterates GitHub search results and returns the first
// open PR that matches the issue by body/title/branch regex.
func (c *Client) findMatchInSearchResults(
	ctx context.Context, issueNum int, issues []*github.Issue,
	bodyRe, titleRe, branchRe *regexp.Regexp,
) *github.PullRequest {
	for _, sr := range issues {
		if sr.PullRequestLinks == nil || sr.GetNumber() == issueNum {
			continue
		}
		pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
		if err != nil {
			continue
		}
		if pr.GetState() == prStateOpen && isMatchForIssue(pr, bodyRe, titleRe, branchRe) {
			c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
			return pr
		}
	}
	return nil
}

// ensureTwoWayLink posts cross-linking comments to the Issue and PR if they
// don't already exist, making it safe to call on every poll iteration.
func (c *Client) ensureTwoWayLink(ctx context.Context, issueNum, prNum int) {
	// Issue side: "Tracking PR #N"
	issueMarker := fmt.Sprintf("%s%d -->", PRLinkCommentMarker, prNum)
	if ok, _, _ := c.HasCommentContaining(ctx, issueNum, issueMarker); !ok {
		body := fmt.Sprintf(
			"copilot-autocode: Tracking PR #%d for this issue.\n%s%d -->",
			prNum, PRLinkCommentMarker, prNum,
		)
		_ = c.PostComment(ctx, issueNum, body)
	}

	// PR side: "addressing Issue #N"
	prMarker := fmt.Sprintf("%s%d -->", IssueLinkCommentMarker, issueNum)
	if ok, _, _ := c.HasCommentContaining(ctx, prNum, prMarker); !ok {
		body := fmt.Sprintf(
			"copilot-autocode: This PR is addressing Issue #%d.\n%s%d -->",
			issueNum, IssueLinkCommentMarker, issueNum,
		)
		_ = c.PostComment(ctx, prNum, body)
	}
}

// IsPRDraft returns true when the PR is still in draft state.
func (c *Client) IsPRDraft(pr *github.PullRequest) bool {
	return pr.GetDraft()
}

// IsBranchBehindBase returns true when the PR branch is behind the base branch.
// It uses the base branch ref name (e.g. "main") rather than the SHA recorded
// on the PR object, which can be stale if the base branch has moved since the
// PR was last synced.
func (c *Client) IsBranchBehindBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	comp, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repo,
		pr.GetBase().GetRef(), pr.GetHead().GetSHA(), nil)
	if err != nil {
		return false, err
	}
	status := comp.GetStatus()
	return status == "behind" || status == "diverged", nil
}

// PostComment posts a plain comment on the given issue/PR number.
func (c *Client) PostComment(ctx context.Context, issueNum int, body string) error {
	_, _, err := c.gh.Issues.CreateComment(ctx, c.owner, c.repo, issueNum, &github.IssueComment{Body: &body})
	return err
}

// PostReviewComment posts a review comment on a PR.
func (c *Client) PostReviewComment(ctx context.Context, prNum int, body string) error {
	event := "COMMENT"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Body:  &body,
		Event: &event,
	})
	return err
}

// CountRefinementPromptsSent returns the number of refinement prompts already
// posted on the given PR by counting reviews containing RefinementCommentMarker.
// Reading the count from GitHub means it survives process restarts.
func (c *Client) CountRefinementPromptsSent(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return 0, err
		}
		for _, r := range reviews {
			if strings.Contains(r.GetBody(), RefinementCommentMarker) {
				count++
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// CountMergeConflictAttempts returns the number of merge-conflict @copilot
// prompts the orchestrator has already posted on the given PR by counting
// issue comments whose body contains MergeConflictCommentMarker.  Reading
// the count from GitHub means it survives process restarts.
func (c *Client) CountMergeConflictAttempts(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return 0, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), MergeConflictCommentMarker) {
				count++
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// ApprovePR approves the PR.
func (c *Client) ApprovePR(ctx context.Context, prNum int) error {
	event := "APPROVE"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Event: &event,
	})
	return err
}

// MergePR merges the PR using the method and commit message from Config.
func (c *Client) MergePR(ctx context.Context, pr *github.PullRequest) error {
	_, _, err := c.gh.PullRequests.Merge(ctx, c.owner, c.repo, pr.GetNumber(),
		c.mergeMsg,
		&github.PullRequestOptions{MergeMethod: c.mergeMethod})
	return err
}

// LatestWorkflowRun returns all recent workflow runs for the given commit SHA.
func (c *Client) LatestWorkflowRun(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: perPageSmall},
	})
	if err != nil {
		return nil, err
	}
	return runs.WorkflowRuns, nil
}

// FailedRunDetails finds the first failed workflow run for a commit SHA and
// returns its display name together with the name and log URL of every failed
// job inside that run.  Returns ("", nil, nil) when no failed run is found.
//
// FailedRunDetails returns the name of the failed workflow and a list of failed
// jobs (with optional log URLs) for the given commit SHA.
//
// This replaces the old FindFailedRunID + FailedRunLogs pair so callers get
// the workflow title in one call, which is included in the @copilot message so
// Copilot knows exactly where to look.
func (c *Client) FailedRunDetails(
	ctx context.Context, sha string,
) (string, []FailedJobInfo, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return "", nil, err
	}

	var workflowName string
	var runID int64
	for _, r := range runs {
		if r.GetConclusion() == "failure" {
			workflowName = r.GetName()
			runID = r.GetID()
			break
		}
	}
	if runID == 0 {
		return "", nil, nil
	}

	jobsResp, _, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID,
		&github.ListWorkflowJobsOptions{
			Filter:      "latest",
			ListOptions: github.ListOptions{PerPage: perPageMedium},
		})
	if err != nil {
		// Return the workflow name even if job details fail.
		return workflowName, nil, err
	}
	var jobs []FailedJobInfo
	for _, job := range jobsResp.Jobs {
		if job.GetConclusion() != "failure" {
			continue
		}
		logURL := ""
		u, _, lerr := c.gh.Actions.GetWorkflowJobLogs(
			ctx, c.owner, c.repo, job.GetID(), maxLogRedirects,
		)
		if lerr == nil {
			logURL = u.String()
		}
		jobs = append(jobs, FailedJobInfo{Name: job.GetName(), LogURL: logURL})
	}
	return workflowName, jobs, nil
}

// AnyWorkflowRunActive returns true when any workflow run on the given SHA is
// still in progress or queued.  This is used as a proxy for "is the Copilot
// agent or CI still working" — the Copilot coding agent does not have a
// dedicated status API, so checking for active workflow runs (triggered by the
// agent's pushes) is the best available signal.
func (c *Client) AnyWorkflowRunActive(ctx context.Context, sha string) (bool, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		s := r.GetStatus()
		if s == "in_progress" || s == "queued" || s == "requested" {
			return true, nil
		}
	}
	return false, nil
}

// HasActiveCopilotRun checks whether there are any in-progress or queued
// workflow runs in the repository that appear to be from the Copilot coding
// agent.  It matches runs whose triggering actor login contains "copilot"
// (case-insensitive) or whose workflow name contains "copilot".
//
// This is used as a guard before re-invoking the agent: if a Copilot run
// is already active, we skip re-invocation to avoid duplicate tasks.
func (c *Client) HasActiveCopilotRun(ctx context.Context) (bool, error) {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		ListOptions: github.ListOptions{PerPage: perPageSmall},
	})
	if err != nil {
		return false, err
	}
	for _, r := range runs.WorkflowRuns {
		status := r.GetStatus()
		if status != "in_progress" && status != "queued" && status != "requested" {
			continue
		}
		actor := strings.ToLower(r.GetActor().GetLogin())
		name := strings.ToLower(r.GetName())
		if strings.Contains(actor, "copilot") || strings.Contains(name, "copilot") {
			return true, nil
		}
	}
	return false, nil
}

// AllRunsSucceeded returns (allSuccess bool, anyFailure bool, err).
func (c *Client) AllRunsSucceeded(ctx context.Context, sha string) (bool, bool, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return false, false, err
	}
	if len(runs) == 0 {
		return true, false, nil // No runs -> no failures -> inherently successful
	}
	allSuccess := true
	anyFailure := false
	for _, r := range runs {
		status := r.GetStatus()
		conclusion := r.GetConclusion()

		// If any run finished with a failure, we flag it immediately
		// so the agent can start fixing it while others are still running.
		if status == runStatusCompleted &&
			(conclusion != "success" && conclusion != "skipped" && conclusion != "neutral") {
			anyFailure = true
			allSuccess = false
		}

		if status != runStatusCompleted {
			allSuccess = false
		}
	}
	return allSuccess, anyFailure, nil
}

// LatestFailedRunConclusion returns the conclusion string (e.g. "failure",
// "timed_out") and completion time of the most recent completed-but-unsuccessful
// workflow run for the given SHA.  Returns ("", zero, nil) when every run
// succeeded/was skipped, or no runs exist at all.
func (c *Client) LatestFailedRunConclusion(ctx context.Context, sha string) (string, time.Time, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return "", time.Time{}, err
	}
	for _, r := range runs {
		if r.GetStatus() != runStatusCompleted {
			continue
		}
		conc := r.GetConclusion()
		// Only treat "timed_out" as an actionable agent failure.
		// "failure" is typically a normal CI test failure which should
		// be handled by the CI-fix feedback loop instead.
		// Other non-success conclusions (cancelled, neutral, stale) are
		// either user-initiated or transient and should not trigger the
		// "@copilot continue" recovery flow.
		if conc == "timed_out" {
			completedAt := r.GetUpdatedAt().Time
			return conc, completedAt, nil
		}
	}
	return "", time.Time{}, nil
}

// ListActionRequiredRuns returns all workflow runs for the given SHA that are
// currently stuck in either the "action_required" or "waiting" status.
// These typically require manual approval to proceed (e.g., from outside collaborators).
func (c *Client) ListActionRequiredRuns(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return nil, err
	}
	var required []*github.WorkflowRun
	for _, r := range runs {
		status := r.GetStatus()
		if status == "action_required" || status == "waiting" {
			required = append(required, r)
		}
	}
	return required, nil
}

// ApproveWorkflowRun sends a raw API request to approve a pending workflow run.
// (go-github v68 does not expose this specific endpoint).
func (c *Client) ApproveWorkflowRun(ctx context.Context, runID int64) error {
	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/runs/%d/approve",
		url.PathEscape(c.owner), url.PathEscape(c.repo), runID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build approve run request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("approve run %d: %w", runID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		log.Printf("Actions: successfully approved run %d", runID)
		return nil
	}
	return fmt.Errorf(
		"approve run %d: unexpected status %d %s",
		runID, resp.StatusCode, http.StatusText(resp.StatusCode),
	)
}

// CountAgentContinueComments returns the number of "@copilot continue"
// comments (identified by AgentContinueCommentMarker) posted on the given
// issue or PR number.  This covers agent-coding timeouts and refinement nudges.
func (c *Client) CountAgentContinueComments(ctx context.Context, num int) (int, error) {
	return c.countCommentsWithMarker(ctx, num, AgentContinueCommentMarker)
}

// LastAgentContinueAt returns the timestamp of the most recent "@copilot
// continue" comment on the given issue or PR, or zero time if none exist.
func (c *Client) LastAgentContinueAt(ctx context.Context, num int) (time.Time, error) {
	return c.lastCommentWithMarker(ctx, num, AgentContinueCommentMarker)
}

// CountMergeConflictContinueComments returns the number of merge-conflict
// nudge comments (identified by MergeConflictContinueCommentMarker) posted on
// the given PR.  This budget is separate from AgentContinueCommentMarker so
// the merge-conflict phase cannot consume the refinement+CI retry allowance.
func (c *Client) CountMergeConflictContinueComments(ctx context.Context, num int) (int, error) {
	return c.countCommentsWithMarker(ctx, num, MergeConflictContinueCommentMarker)
}

// LastMergeConflictContinueAt returns the timestamp of the most recent
// merge-conflict nudge comment, or zero time if none exist.
func (c *Client) LastMergeConflictContinueAt(ctx context.Context, num int) (time.Time, error) {
	return c.lastCommentWithMarker(ctx, num, MergeConflictContinueCommentMarker)
}

// countCommentsWithMarker counts issue/PR comments containing the given marker.
func (c *Client) countCommentsWithMarker(ctx context.Context, num int, marker string) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return 0, fmt.Errorf("list comments (#%d): %w", num, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), marker) {
				count++
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// lastCommentWithMarker returns the timestamp of the most recent issue/PR
// comment containing the given marker, or zero time if none exist.
func (c *Client) lastCommentWithMarker(ctx context.Context, num int, marker string) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list comments (#%d): %w", num, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), marker) {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// EnsureLabelsExist creates any missing ai-* labels with default colours.
func (c *Client) EnsureLabelsExist(ctx context.Context) error {
	needed := map[string]string{
		c.labelQueue:  "0075ca",
		c.labelCoding: "e4e669",
		c.labelReview: "d93f0b",
	}
	existing, _, err := c.gh.Issues.ListLabels(ctx, c.owner, c.repo, &github.ListOptions{PerPage: perPageDefault})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, l := range existing {
		have[l.GetName()] = true
	}
	for name, color := range needed {
		if have[name] {
			continue
		}
		n, col := name, color
		if _, _, err := c.gh.Issues.CreateLabel(ctx, c.owner, c.repo, &github.Label{
			Name:  &n,
			Color: &col,
		}); err != nil {
			return fmt.Errorf("create label %q: %w", name, err)
		}
	}
	return nil
}

// MergedPRForIssue finds the first *merged* PR whose title or body references the issue.
func (c *Client) MergedPRForIssue(ctx context.Context, issue *github.Issue) (*github.PullRequest, error) {
	issueNum := issue.GetNumber()
	query := fmt.Sprintf("repo:%s/%s is:pr is:merged %d", c.owner, c.repo, issueNum)
	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: perPageMin},
	}
	result, _, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	bodyRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|\s)(?:fixes|resolves|closes)\s+#%d\b`, issueNum))
	titleRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|\s)#%d\b`, issueNum))
	branchRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|/)(?:issue-?)?%d(?:[-/]|$)`, issueNum))

	for _, sr := range result.Issues {
		if sr.PullRequestLinks != nil && sr.GetNumber() != issueNum {
			pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
			if err != nil {
				continue
			}
			if pr.GetMerged() {
				if isMatchForIssue(pr, bodyRe, titleRe, branchRe) {
					return pr, nil
				}
			}
		}
	}

	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State:       "closed",
		ListOptions: github.ListOptions{PerPage: perPageMedium},
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if !pr.GetMerged() {
			continue
		}
		if isMatchForIssue(pr, bodyRe, titleRe, branchRe) {
			return pr, nil
		}
	}
	return nil, nil
}

// PRIsUpToDateWithBase returns true when the PR has no merge conflicts and is
// not behind the base branch.
func (c *Client) PRIsUpToDateWithBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	if pr.GetMergeableState() == "behind" || pr.GetMergeableState() == "dirty" {
		return false, nil
	}
	behind, err := c.IsBranchBehindBase(ctx, pr)
	if err != nil {
		return false, err
	}
	return !behind, nil
}

// CodingLabeledAt returns the most recent time the given label was applied to
// the issue by scanning the issue's event timeline.  Returns zero time if no
// such event is found.
func (c *Client) CodingLabeledAt(ctx context.Context, issueNum int, label string) (time.Time, error) {
	var latest time.Time
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		events, resp, err := c.gh.Issues.ListIssueEvents(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list issue events (#%d): %w", issueNum, err)
		}
		for _, e := range events {
			if e.GetEvent() == "labeled" && e.GetLabel().GetName() == label {
				if t := e.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// CountNudgesSince returns the number of nudge comments posted on the issue
// after the given time.  Pass zero time to count all nudge comments.
func (c *Client) CountNudgesSince(ctx context.Context, issueNum int, since time.Time) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return 0, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), CopilotNudgeCommentMarker) {
				if since.IsZero() || !cm.GetCreatedAt().Time.Before(since) {
					count++
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// LastNudgeAt returns the timestamp of the most recent nudge comment on the
// issue, or zero time if none exist.
func (c *Client) LastNudgeAt(ctx context.Context, issueNum int) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), CopilotNudgeCommentMarker) {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// TimeAgo returns a short human-readable relative time string.
func TimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/housPerDay))
	}
}

// CopilotAgentJobRequest is the POST body sent to the Copilot API when
// creating a new coding-agent task.
type CopilotAgentJobRequest struct {
	Title            string `json:"title,omitempty"`
	ProblemStatement string `json:"problem_statement"`
	EventType        string `json:"event_type"`
	IssueNumber      int    `json:"issue_number,omitempty"`
	IssueURL         string `json:"issue_url,omitempty"`
}

// Copilot agent task (e.g., "in_progress", "running", "queued", "completed", "failed").
type CopilotJobStatus struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"` // "running", "completed", etc.
}

// GetCopilotJobStatus queries the Copilot API for the status of a specific job ID.
func (c *Client) GetCopilotJobStatus(ctx context.Context, jobID string) (*CopilotJobStatus, error) {
	endpoint := fmt.Sprintf(
		"%s/agents/swe/v1/jobs/%s/%s/%s",
		copilotAPIBase,
		url.PathEscape(c.owner),
		url.PathEscape(c.repo),
		url.PathEscape(jobID),
	)
	return c.GetJobStatusAt(ctx, endpoint, jobID)
}

// GetJobStatusAt is the testable core of GetCopilotJobStatus.
func (c *Client) GetJobStatusAt(ctx context.Context, endpoint, jobID string) (*CopilotJobStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build copilot job status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Copilot-Integration-Id", "copilot-autocode")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoke copilot job status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("copilot job %s not found (404)", jobID)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get copilot job status %s: unexpected status %d %s: %s",
			jobID, resp.StatusCode, http.StatusText(resp.StatusCode), string(respBody))
	}

	var status CopilotJobStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode copilot job status response: %w", err)
	}
	return &status, nil
}

// LatestCopilotJobID returns the most recent Copilot task job ID recorded on the
// issue, or an empty string if none exist.
func (c *Client) LatestCopilotJobID(ctx context.Context, issueNum int) (string, error) {
	var latestJobID string
	var latest time.Time

	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return "", fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			body := cm.GetBody()
			idx := strings.Index(body, CopilotJobIDCommentMarker)
			if idx != -1 {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					start := idx + len(CopilotJobIDCommentMarker)
					end := strings.Index(body[start:], "-->")
					if end != -1 {
						latestJobID = strings.TrimSpace(body[start : start+end])
						latest = t
					}
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latestJobID, nil
}

// InvokeCopilotAgent creates a new Copilot coding-agent task by calling the
// Copilot API directly — the same backend that powers the GitHub Agents tab
// and the `gh agent-task create` CLI command.
//
// The issueNum and issueURL parameters link the agent task to the originating
// issue so the agent creates a PR that references it.  This bypasses the
// unreliable issue-assignment and @-mention trigger mechanisms. We also explicitly
// inject a "Fixes #issueNum" string into the prompt so that the Copilot workspace agent
// will reliably use the issue in branch strings and link it locally.
//
// Returns the job ID from the CAPI response (empty string if not parseable).
func (c *Client) InvokeCopilotAgent(
	ctx context.Context, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	prompt = fmt.Sprintf("%s\n\nFixes #%d", prompt, issueNum)
	endpoint := fmt.Sprintf(
		"%s/agents/swe/v1/jobs/%s/%s",
		copilotAPIBase,
		url.PathEscape(c.owner),
		url.PathEscape(c.repo),
	)
	return c.InvokeAgentAt(ctx, endpoint, prompt, issueTitle, issueNum, issueURL)
}

// copilotAgentJobResponse captures the fields we care about from the CAPI
// response.  The actual schema may contain more fields; we only parse what
// we need and log the full body for discovery.
type copilotAgentJobResponse struct {
	ID    string `json:"id"`
	JobID string `json:"job_id"`
}

// InvokeAgentAt is the testable core of InvokeCopilotAgent.  It accepts an
// explicit URL so tests can point it at an [httptest.Server].
//
// Returns the job ID extracted from the response (best-effort; empty if the
// response doesn't contain a recognisable ID field).
func (c *Client) InvokeAgentAt(
	ctx context.Context, endpoint, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	body, err := json.Marshal(&CopilotAgentJobRequest{
		Title:            fmt.Sprintf("[copilot-autocode] #%d: %s", issueNum, issueTitle),
		ProblemStatement: prompt,
		EventType:        "copilot-autocode",
		IssueNumber:      issueNum,
		IssueURL:         issueURL,
	})
	if err != nil {
		return "", fmt.Errorf("marshal copilot agent request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build copilot agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Copilot-Integration-Id", "copilot-autocode")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("invoke copilot agent: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invoke copilot agent: unexpected status %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), string(respBody))
	}

	// Best-effort parse of the job ID from the response.
	var parsed copilotAgentJobResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		log.Printf("copilot agent: could not parse response body: %v (raw: %s)", err, string(respBody))
		return "", nil
	}
	jobID := parsed.ID
	if jobID == "" {
		jobID = parsed.JobID
	}
	log.Printf("copilot agent: invoked successfully (job_id=%q, raw response: %s)", jobID, string(respBody))
	return jobID, nil
}

// MarkPRReady removes draft status from a pull request using the GitHub
// GraphQL API (the REST API does not support changing draft status).
func (c *Client) MarkPRReady(ctx context.Context, pr *github.PullRequest) error {
	nodeID := pr.GetNodeID()
	if nodeID == "" {
		return fmt.Errorf("PR #%d has no node ID", pr.GetNumber())
	}

	const mutation = `mutation($id: ID!) ` +
		`{ markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { id } } }`
	payload := map[string]any{
		"query":     mutation,
		"variables": map[string]string{"id": nodeID},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mark PR ready: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mark PR ready: unexpected status %d %s",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}

// HasReviewContaining returns true and the timestamp if any review comment on the PR contains
// the given substring.  Used for SHA-based deduplication of CI-fix and
// refinement prompts.
func (c *Client) HasReviewContaining(ctx context.Context, prNum int, needle string) (bool, time.Time, error) {
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return false, time.Time{}, err
		}
		for _, r := range reviews {
			if strings.Contains(r.GetBody(), needle) {
				return true, r.GetSubmittedAt().Time, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, time.Time{}, nil
}

// HasCommentContaining returns true and the timestamp if any issue comment on the given
// issue/PR number contains the given substring.
func (c *Client) HasCommentContaining(ctx context.Context, num int, needle string) (bool, time.Time, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return false, time.Time{}, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), needle) {
				return true, cm.GetCreatedAt().Time, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, time.Time{}, nil
}

// SHAMarker returns a marker string that embeds a commit SHA prefix, used
// to deduplicate per-SHA comments across poll ticks.
func SHAMarker(prefix, sha string) string {
	short := sha
	if len(short) > shortSHALen {
		short = short[:7]
	}
	return fmt.Sprintf("<!-- copilot-autocode:%s:%s -->", prefix, short)
}
