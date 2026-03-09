// Package poller implements the state machine that drives the Copilot
// Orchestrator workflow.  It runs as a background goroutine and uses GitHub
// labels, PR states, and workflow-run statuses as the single source of truth.
package poller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/go-github/v68/github"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/pkgs/logger"
	"github.com/BlackbirdWorks/copilot-autocode/resolver"
)

// issueDisplayInfo holds the human-readable status for one issue, computed
// entirely from live GitHub data during a single tick (never persisted between
// ticks).  This keeps the app stateless: restarting it produces exactly the
// same display because all data is re-read from GitHub.
type issueDisplayInfo struct {
	current         string              // e.g. "Waiting on coding agent to start"
	next            string              // e.g. "Poke Copilot"
	nextActionAt    time.Time           // when 'next' fires; zero = no countdown
	pr              *github.PullRequest // cached PR lookup (nil = no PR found)
	refinementCount int
	refinementMax   int
	agentStatus     string // "pending" | "success" | "failed"
}

// State is the poller's high-level understanding of a single issue.
type State struct {
	Issue           *github.Issue
	PR              *github.PullRequest
	Status          string // "queue" | "coding" | "review"
	Message         string // last action taken
	CurrentStatus   string // human-readable current phase
	NextAction      string // human-readable next action label
	NextActionAt    time.Time
	RefinementCount int
	RefinementMax   int
	AgentStatus     string // "pending" | "success" | "failed"
}

// Event is sent on the Events channel after every poll tick.
type Event struct {
	Queue    []*State
	Coding   []*State
	Review   []*State
	LastRun  time.Time
	Err      error
	Warnings []string // non-fatal warnings, e.g. Copilot assignment failures
}

// Poller orchestrates the Copilot workflow state machine.
type Poller struct {
	cfg            *config.Config
	gh             *ghclient.Client
	token          string // GitHub PAT — used only for local git operations
	Events         chan Event
	approveRetries map[int64]int
	mu             sync.Mutex // protects approveRetries across concurrent processOne calls
}

// New creates a Poller ready to Start.
func New(cfg *config.Config, gh *ghclient.Client, token string) *Poller {
	return &Poller{
		cfg:            cfg,
		gh:             gh,
		token:          token,
		Events:         make(chan Event, 1),
		approveRetries: make(map[int64]int),
	}
}

// Cfg returns the config used by this Poller, exposed for testing.
func (p *Poller) Cfg() *config.Config { return p.cfg }

// Start launches the polling goroutine.  It runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		// Ensure labels exist before the first tick.
		if err := p.gh.EnsureLabelsExist(ctx); err != nil {
			logger.Load(ctx).WarnContext(ctx, "could not ensure labels exist", slog.Any("err", err))
		}

		// Run once immediately in the background so the UI doesn't hang.
		p.tick(ctx)

		ticker := time.NewTicker(time.Duration(p.cfg.PollIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.tick(ctx)
			}
		}
	}()
}

// fetchAllIssues returns the queue, coding, and reviewing issue lists fetched
// from GitHub in a single concurrent batch — three parallel API calls instead
// of the up-to-six sequential calls that the old per-phase fetch pattern used.
func (p *Poller) fetchAllIssues(ctx context.Context) ([]*github.Issue, []*github.Issue, []*github.Issue, error) {
	type result struct {
		issues []*github.Issue
		err    error
	}
	labels := [3]string{p.cfg.LabelQueue, p.cfg.LabelCoding, p.cfg.LabelReview}
	results := [3]result{}
	var wg sync.WaitGroup
	for i, label := range labels {
		wg.Go(func() {
			issues, fetchErr := p.gh.IssuesByLabel(ctx, label)
			results[i] = result{issues, fetchErr}
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			return nil, nil, nil, fmt.Errorf("fetch %s issues: %w", labels[i], r.err)
		}
	}
	return results[0].issues, results[1].issues, results[2].issues, nil
}

// tick executes one full state-machine cycle and sends an Event.
func (p *Poller) tick(ctx context.Context) {
	evt := Event{LastRun: time.Now()}

	// Bulk-fetch all tracked issue lists in a single concurrent batch — three
	// parallel API calls instead of the old pattern of re-fetching per phase.
	queue, coding, reviewing, err := p.fetchAllIssues(ctx)
	if err != nil {
		evt.Err = err
		select {
		case p.Events <- evt:
		default:
			<-p.Events
			p.Events <- evt
		}
		return
	}

	// Deduplicate across lists: if an issue has multiple phase labels (e.g.
	// from a partial SwapLabel), process it only in the most-advanced phase.
	// Priority: reviewing > coding > queue.
	coding, reviewing = deduplicateIssueLists(queue, coding, reviewing)

	// Phase 1: Queue → Coding (sequential — slot counting must be consistent).
	if err := p.promoteFromQueue(ctx, queue, coding, reviewing); err != nil {
		evt.Err = fmt.Errorf("promote from queue: %w", err)
	}

	// Phases 2+2.5 and 3+: each issue is processed in its own goroutine so
	// independent issues make progress concurrently.  Each goroutine writes
	// into a private displayInfo map; the results are merged after all workers
	// finish to avoid map-write races.
	type diEntry struct {
		num  int
		info *issueDisplayInfo
	}
	entries := make(chan diEntry, len(coding)+len(reviewing))

	var wg sync.WaitGroup

	// Phase 2+2.5: coding issues (transition + nudge).
	for _, issue := range coding {
		wg.Go(func() {
			local := make(map[int]*issueDisplayInfo)
			if gErr := p.processCodingIssue(ctx, issue, local); gErr != nil {
				logger.Load(ctx).ErrorContext(ctx, "error processing coding issue",
					slog.Int("issue", issue.GetNumber()), slog.Any("err", gErr))
			}
			for k, v := range local {
				entries <- diEntry{k, v}
			}
		})
	}

	// Phase 3+: review PRs (merge-conflict → CI → refinement → merge).
	for _, issue := range reviewing {
		wg.Go(func() {
			local := make(map[int]*issueDisplayInfo)
			if gErr := p.processOne(ctx, issue, local); gErr != nil {
				logger.Load(ctx).ErrorContext(ctx, "error processing issue",
					slog.Int("issue", issue.GetNumber()), slog.Any("err", gErr))
			}
			for k, v := range local {
				entries <- diEntry{k, v}
			}
		})
	}

	wg.Wait()
	close(entries)

	displayInfo := make(map[int]*issueDisplayInfo, len(coding)+len(reviewing))
	for e := range entries {
		displayInfo[e.num] = e.info
	}

	// Collect current snapshot for the TUI, enriched with per-issue status.
	evt.Queue, evt.Coding, evt.Review = p.snapshot(ctx, displayInfo)

	// Non-blocking send; drop stale event if channel is full.
	select {
	case p.Events <- evt:
	default:
		<-p.Events
		p.Events <- evt
	}
}

// promoteFromQueue moves issues from ai-queue → ai-coding up to the
// concurrency limit.  The caller must pass the already-fetched queue, coding,
// and reviewing slices so that this phase reuses the bulk-loaded data rather
// than issuing redundant API calls.
func (p *Poller) promoteFromQueue(
	ctx context.Context,
	queue, coding, reviewing []*github.Issue,
) error {
	slots := p.cfg.MaxConcurrentIssues - (len(coding) + len(reviewing))
	if slots <= 0 {
		return nil
	}

	// Process oldest-first (lowest issue number = opened earliest).
	SortIssuesAsc(queue)

	for i := 0; i < slots && i < len(queue); i++ {
		issue := queue[i]
		num := issue.GetNumber()

		// 1. Double check for an existing PR. If the app crashed while the agent
		// was coding, we might find a PR already exists upon restart.
		existingPR, err := p.gh.OpenPRForIssue(ctx, issue)
		if err == nil && existingPR != nil {
			logger.Load(ctx).InfoContext(ctx, "found existing PR; skipping invoke and entering coding flow",
				slog.Int("issue", num), slog.Int("pr", existingPR.GetNumber()))
			if err := p.gh.SwapLabel(ctx, num, p.cfg.LabelQueue, p.cfg.LabelCoding); err != nil {
				return err
			}
			continue
		}

		// 2. No PR found — switch to coding and invoke the agent directly via
		// the Copilot API (the same backend as the GitHub Agents tab).
		if err := p.gh.SwapLabel(ctx, num, p.cfg.LabelQueue, p.cfg.LabelCoding); err != nil {
			return err
		}
		prompt := FormatFallbackPrompt(p.cfg.FallbackIssueInvokePrompt, issue)
		jobID, capiErr := p.gh.InvokeCopilotAgent(ctx, prompt, issue.GetTitle(), num, issue.GetHTMLURL())
		if capiErr != nil {
			logger.Load(ctx).ErrorContext(ctx, "could not invoke copilot agent via CAPI",
				slog.Int("issue", num), slog.Any("err", capiErr))
		}

		// Post a silent tracking comment (no @-mention — the CAPI call above
		// is the actual trigger). The embedded marker lets CountNudgesSince
		// and LastNudgeAt reconstruct the attempt count and last-activity
		// timestamp after a process restart.  The job-id marker records the
		// CAPI task ID so we can avoid duplicate invocations.
		comment := fmt.Sprintf(
			"copilot-autocode: agent task created for issue #%d (initial invoke).\n%s",
			num, ghclient.CopilotNudgeCommentMarker,
		)
		if jobID != "" {
			comment += fmt.Sprintf("\n%s%s -->", ghclient.CopilotJobIDCommentMarker, jobID)
		}
		if err := p.gh.PostComment(ctx, num, comment); err != nil {
			logger.Load(ctx).ErrorContext(ctx, "could not post invoke tracking comment",
				slog.Int("issue", num), slog.Any("err", err))
		}
	}
	return nil
}

// processCodingIssue handles a single ai-coding issue in one goroutine.
// It consolidates what was previously the moveCodingToReview loop body and
// the nudgeSingleCodingIssue logic into a single per-issue function so that
// every coding issue can be processed concurrently without repeating the bulk
// issue-list fetch.
func (p *Poller) processCodingIssue(
	ctx context.Context, issue *github.Issue,
	displayInfo map[int]*issueDisplayInfo,
) error {
	num := issue.GetNumber()
	pr, err := p.gh.OpenPRForIssue(ctx, issue)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not look up PR for coding issue",
			slog.Int("issue", num), slog.Any("err", err))
	}
	if pr != nil {
		return p.processCodingPR(ctx, pr, num, displayInfo)
	}

	// No open PR — check if one was already merged externally before
	// re-invoking the agent (avoids creating duplicate PRs).
	mergedPR, mErr := p.gh.MergedPRForIssue(ctx, issue)
	if mErr == nil && mergedPR != nil {
		logger.Load(ctx).InfoContext(ctx, "PR merged externally; closing issue",
			slog.Int("issue", num), slog.Int("pr", mergedPR.GetNumber()))
		displayInfo[num] = &issueDisplayInfo{
			current:     "PR merged externally — closing issue",
			pr:          mergedPR,
			agentStatus: "success",
		}
		_ = p.gh.RemoveLabel(ctx, num, p.cfg.LabelCoding)
		return p.gh.CloseIssue(ctx, num)
	}

	// No PR yet — run the timeout/nudge logic.
	return p.nudgeSingleCodingIssue(
		ctx, issue, displayInfo,
		time.Duration(p.cfg.CopilotInvokeTimeoutSeconds)*time.Second,
	)
}

// processCodingPR handles the PR-present path of processCodingIssue:
// it waits for active runs, promotes draft PRs, or transitions to review.
func (p *Poller) processCodingPR(
	ctx context.Context, pr *github.PullRequest, num int,
	displayInfo map[int]*issueDisplayInfo,
) error {
	sha := pr.GetHead().GetSHA()
	running, err := p.gh.AnyWorkflowRunActive(ctx, sha)
	if err == nil && running {
		label := "Agent finalizing code"
		if p.gh.IsPRDraft(pr) {
			label = "Copilot is writing code"
		}
		displayInfo[num] = &issueDisplayInfo{
			current:     label,
			next:        "Waiting for agent to complete",
			pr:          pr,
			agentStatus: "pending",
		}
		return nil
	}
	if p.gh.IsPRDraft(pr) {
		return p.processDraftPR(ctx, pr, num, displayInfo)
	}
	// PR is ready — transition to review.
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelReview)
}

// processDraftPR handles the draft-PR case in processCodingPR: the agent
// has completed its coding push. It checks for agent failures first, then
// marks the PR as ready and transitions the issue to ai-review.
func (p *Poller) processDraftPR(
	ctx context.Context, pr *github.PullRequest, num int,
	displayInfo map[int]*issueDisplayInfo,
) error {
	// Check if the agent run failed or timed out before advancing to review.
	if handled, err := p.handleAgentFailure(ctx, pr, num, displayInfo); err != nil {
		return err
	} else if handled {
		return nil
	}
	// Agent finished its draft push — promote unconditionally.
	logger.Load(ctx).InfoContext(ctx, "agent finished draft push; marking ready", slog.Int("pr", pr.GetNumber()))
	if err := p.gh.MarkPRReady(ctx, pr); err != nil {
		// Don't transition to review if PR stays draft — merge will fail.
		logger.Load(ctx).WarnContext(ctx, "could not mark PR as ready; staying in coding",
			slog.Int("pr", pr.GetNumber()), slog.Any("err", err))
		displayInfo[num] = &issueDisplayInfo{
			current:     "Agent completed but PR still draft — retrying",
			next:        "Retry mark ready next poll",
			pr:          pr,
			agentStatus: "pending",
		}
		return nil
	}
	displayInfo[num] = &issueDisplayInfo{
		current: "Agent completed — PR marked ready",
		next:    "Transitioning to review",
		pr:      pr,
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelReview)
}

// nudgeSingleCodingIssue is called by processCodingIssue when no PR exists
// yet; it handles the timeout/retry/nudge flow for a single coding issue.
// The caller has already confirmed that no open PR exists for this issue.
func (p *Poller) nudgeSingleCodingIssue(
	ctx context.Context, issue *github.Issue,
	displayInfo map[int]*issueDisplayInfo,
	timeout time.Duration,
) error {
	num := issue.GetNumber()
	logger.Load(ctx).InfoContext(ctx, "nudgeSingleCodingIssue: starting check", slog.Int("issue", num))

	displayInfo[num] = &issueDisplayInfo{
		current: "Agent assigned, awaiting PR",
		next:    "Nudge if no PR",
	}

	codingAt, err := p.gh.CodingLabeledAt(ctx, num, p.cfg.LabelCoding)
	if err != nil || codingAt.IsZero() {
		logger.Load(ctx).InfoContext(ctx, "nudge check: coding label time is zero or error",
			slog.Int("issue", num),
			slog.Any("err", err),
			slog.Time("codingAt", codingAt),
		)
		if err != nil {
			logger.Load(ctx).WarnContext(ctx, "nudge check: could not determine coding label time",
				slog.Int("issue", num), slog.Any("err", err))
		}
		return nil
	}

	nudgeCount, err := p.gh.CountNudgesSince(ctx, num, codingAt)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "nudge check: could not count nudges",
			slog.Int("issue", num), slog.Any("err", err))
		return nil
	}

	lastNudge, err := p.gh.LastNudgeAt(ctx, num)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "nudge check: could not fetch last nudge time",
			slog.Int("issue", num), slog.Any("err", err))
		return nil
	}

	lastActivity := codingAt
	if lastNudge.After(lastActivity) {
		lastActivity = lastNudge
	}
	// Guard against zero-value timestamps from missing label events.
	if lastActivity.IsZero() {
		lastActivity = time.Now()
	}

	deadline := lastActivity.Add(timeout)
	if time.Since(lastActivity) < timeout {
		statusLabel := "Agent assigned, awaiting PR"
		if nudgeCount > 0 {
			statusLabel = fmt.Sprintf("Agent invoked via API (attempt %d)", nudgeCount+1)
		}
		displayInfo[num] = &issueDisplayInfo{
			current:      statusLabel,
			next:         "Nudge if no PR",
			nextActionAt: deadline,
			agentStatus:  "pending",
		}
		return nil
	}

	if nudgeCount >= p.cfg.CopilotInvokeMaxRetries {
		return p.handleNudgeExhaustion(ctx, num, nudgeCount, displayInfo)
	}

	// Double check before invoking — a PR might have appeared since the last pass.
	// OpenPRForIssue calls ensureTwoWayLink internally when it discovers a PR,
	// so finding it here is sufficient to post the cross-link comments.
	if pr, err := p.gh.OpenPRForIssue(ctx, issue); err == nil && pr != nil {
		logger.Load(ctx).InfoContext(ctx, "PR found just before nudge; skipping nudge",
			slog.Int("issue", num), slog.Int("pr", pr.GetNumber()))
		return nil
	}

	if p.isAgentActive(ctx, num) {
		displayInfo[num] = &issueDisplayInfo{
			current:     "Agent actively running — waiting",
			next:        "Re-check next poll",
			agentStatus: "pending",
		}
		return nil
	}

	return p.requestNudge(ctx, issue, num, nudgeCount, timeout, displayInfo)
}

// isAgentActive returns true if either a specific Copilot Job is in progress
// or a general repository-level Copilot run is active.
func (p *Poller) isAgentActive(ctx context.Context, num int) bool {
	// 1. Try to check the specific CAPI Job ID if we have one.
	jobID, _ := p.gh.LatestCopilotJobID(ctx, num)
	if jobID != "" {
		status, err := p.gh.GetCopilotJobStatus(ctx, jobID)
		if err == nil && status != nil {
			s := status.Status
			if s == "in_progress" || s == "running" || s == "queued" || s == "requested" || s == "pending" {
				return true
			}
		} else {
			logger.Load(ctx).WarnContext(ctx, "could not fetch status for job; falling back",
				slog.String("job", jobID), slog.Any("err", err))
		}
	}

	// 2. Fallback to the repo-level workflow run check.
	if active, aErr := p.gh.HasActiveCopilotRun(ctx); aErr == nil && active {
		logger.Load(ctx).InfoContext(ctx, "active Copilot workflow run detected; skipping re-invocation",
			slog.Int("issue", num))
		return true
	}
	return false
}

// requestNudge invokes the agent via the Copilot API and posts a tracking comment.
func (p *Poller) requestNudge(
	ctx context.Context, issue *github.Issue, num, nudgeCount int,
	timeout time.Duration, displayInfo map[int]*issueDisplayInfo,
) error {
	logger.Load(ctx).InfoContext(ctx, "no Copilot activity detected; invoking agent via Copilot API",
		slog.Int("issue", num), slog.Duration("timeout", timeout), slog.Int("attempt", nudgeCount+1))

	displayInfo[num] = &issueDisplayInfo{
		current: fmt.Sprintf(
			"Invoking agent via Copilot API (attempt %d of %d)",
			nudgeCount+1, p.cfg.CopilotInvokeMaxRetries,
		),
		next: "Waiting for response",
	}

	nudgeBody := FormatFallbackPrompt(p.cfg.FallbackIssueInvokePrompt, issue)
	jobID, invokeErr := p.gh.InvokeCopilotAgent(ctx, nudgeBody, issue.GetTitle(), num, issue.GetHTMLURL())
	if invokeErr != nil {
		logger.Load(ctx).ErrorContext(ctx, "could not invoke copilot agent via Copilot API",
			slog.Int("issue", num), slog.Any("err", invokeErr))
	}

	comment := fmt.Sprintf(
		"copilot-autocode: agent task created for issue #%d (attempt %d of %d).\n%s",
		num, nudgeCount+1, p.cfg.CopilotInvokeMaxRetries,
		ghclient.CopilotNudgeCommentMarker,
	)
	if jobID != "" {
		comment += fmt.Sprintf("\n%s%s -->", ghclient.CopilotJobIDCommentMarker, jobID)
	}
	return p.gh.PostComment(ctx, num, comment)
}

// agentTimeoutCfg holds variant-specific parameters for handleAgentTimeout.
type agentTimeoutCfg struct {
	countFn      func(ctx context.Context, prNum int) (int, error)
	nudgeMarker  string
	promptKind   string // e.g. "merge-conflict prompt" or "refinement prompt"
	noticeFormat string // printf format with single %d for continue count
	statusVerb   string // "nudges" or "retries" (used in display status)
}

// handleAgentTimeout checks whether lastActivity is older than the configured
// CopilotInvokeTimeoutSeconds and, if so, either nudges the agent or returns
// the issue to the queue when retries are exhausted.
//
// Returns true when the caller should stop processing the current issue.
func (p *Poller) handleAgentTimeout(
	ctx context.Context, pr *github.PullRequest, num int,
	lastActivity time.Time, cfg agentTimeoutCfg,
	displayInfo map[int]*issueDisplayInfo,
) bool {
	if time.Since(lastActivity) <= time.Duration(p.cfg.CopilotInvokeTimeoutSeconds)*time.Second {
		return false
	}
	continueCount, _ := cfg.countFn(ctx, pr.GetNumber())
	if continueCount >= p.cfg.MaxAgentContinueRetries {
		logger.Load(ctx).InfoContext(ctx, "agent unresponsive; leaving PR in review",
			slog.Int("pr", pr.GetNumber()), slog.String("kind", cfg.promptKind), slog.Int("attempts", continueCount))
		_ = p.gh.PostComment(ctx, num, fmt.Sprintf(cfg.noticeFormat, continueCount))
		displayInfo[num] = &issueDisplayInfo{
			current: fmt.Sprintf(
				"Agent unresponsive after %d %s — left in review",
				continueCount, cfg.statusVerb,
			),
			pr:          pr,
			agentStatus: "failed",
		}
		return true
	}
	logger.Load(ctx).InfoContext(ctx, "agent stuck; nudging",
		slog.Int("pr", pr.GetNumber()),
		slog.String("kind", cfg.promptKind),
		slog.Int("timeout", p.cfg.CopilotInvokeTimeoutSeconds))
	_ = p.gh.PostComment(ctx, pr.GetNumber(), p.cfg.AgentContinuePrompt+"\n"+cfg.nudgeMarker)
	return false
}

// handleMergeConflict manages the merge-conflict/behind-base step of processOne.
// It returns (true, nil) when the caller should exit early (returning nil),
// (false, nil) when the PR is up to date and processing should continue,
// or (false, err) on unexpected errors.
func (p *Poller) handleMergeConflict(
	ctx context.Context, pr *github.PullRequest, num int, sha string,
	displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	upToDate, err := p.gh.PRIsUpToDateWithBase(ctx, pr)
	if err != nil {
		return false, err
	}
	if upToDate {
		return false, nil
	}

	// Always prefer native GitHub "Update branch" if there are no explicit conflicts.
	// If MergeableState is "dirty", we have real conflicts to solve.
	// Otherwise, we attempt to sync from base via API first.
	if pr.GetMergeableState() != "dirty" {
		logger.Load(ctx).InfoContext(ctx, "branch is behind base; attempting native update",
			slog.Int("pr", pr.GetNumber()))
		displayInfo[num] = &issueDisplayInfo{
			current:     "Updating branch from base",
			next:        "Waiting for update",
			pr:          pr,
			agentStatus: "pending",
		}
		if err := p.gh.UpdatePRBranch(ctx, pr.GetNumber()); err != nil {
			logger.Load(ctx).WarnContext(ctx, "failed to update PR branch",
				slog.Int("pr", pr.GetNumber()), slog.Any("err", err))
			// fallback to Copilot/Local AI if the API call itself fails
		} else {
			return true, nil // wait for the update to complete and trigger new CI
		}
	}

	// Wait while the agent is already working on conflicts.
	if running, _ := p.gh.AnyWorkflowRunActive(ctx, sha); running {
		displayInfo[num] = &issueDisplayInfo{
			current:     "Agent resolving merge conflicts",
			next:        "Waiting for agent",
			pr:          pr,
			agentStatus: "pending",
		}
		return true, nil
	}

	attempts, err := p.gh.CountMergeConflictAttempts(ctx, pr.GetNumber())
	if err != nil {
		return false, err
	}

	if attempts >= p.cfg.MaxMergeConflictRetries {
		return p.handleExhaustedMergeConflict(ctx, pr, num, attempts, sha, displayInfo)
	}

	// Still within @copilot retry budget — ask it to fix conflicts.
	return p.requestMergeConflictFix(ctx, pr, num, sha, displayInfo)
}

// handleExhaustedMergeConflict handles the case where all @copilot merge-conflict
// retries have been used up, falling back to local AI resolution if configured.
func (p *Poller) handleExhaustedMergeConflict(
	ctx context.Context, pr *github.PullRequest, num, attempts int, _ string,
	displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	alreadyTried, _, _ := p.gh.HasCommentContaining(ctx, pr.GetNumber(), ghclient.LocalResolutionCommentMarker)
	if alreadyTried {
		displayInfo[num] = &issueDisplayInfo{
			current:     "Merge conflicts unresolved — needs manual fix",
			pr:          pr,
			agentStatus: "failed",
		}
		return true, nil
	}

	logger.Load(ctx).InfoContext(ctx, "merge-conflict attempts exhausted; running local AI resolution",
		slog.Int("pr", pr.GetNumber()),
		slog.Int("attempts", attempts),
		slog.String("resolver", p.cfg.AIMergeResolverCmd))
	displayInfo[num] = &issueDisplayInfo{
		current:     "Running local AI merge resolution",
		next:        "Pushing resolved changes",
		pr:          pr,
		agentStatus: "pending",
	}
	prd := resolver.PRDetails{
		Owner:      p.cfg.GitHubOwner,
		Repo:       p.cfg.GitHubRepo,
		HeadBranch: pr.GetHead().GetRef(),
		BaseBranch: pr.GetBase().GetRef(),
	}
	if err := resolver.New().RunLocalResolution(ctx, p.token, prd, p.cfg); err != nil {
		logger.Load(ctx).ErrorContext(ctx, "local AI merge resolution failed",
			slog.Int("pr", pr.GetNumber()), slog.Any("err", err))
		notice := fmt.Sprintf(
			"copilot-autocode: local AI merge resolution via `%s` failed. "+
				"Manual conflict resolution is required.\n%s",
			p.cfg.AIMergeResolverCmd, ghclient.LocalResolutionCommentMarker)
		_ = p.gh.PostComment(ctx, pr.GetNumber(), notice)
		return true, nil
	}
	notice := fmt.Sprintf(
		"copilot-autocode: Merge conflicts were resolved locally by copilot-autocode using `%s`.\n%s",
		p.cfg.AIMergeResolverCmd, ghclient.LocalResolutionCommentMarker)
	_ = p.gh.PostComment(ctx, pr.GetNumber(), notice)
	return true, nil
}

// requestMergeConflictFix sends or nudges a @copilot merge-conflict fix request.
func (p *Poller) requestMergeConflictFix(
	ctx context.Context, pr *github.PullRequest, num int, sha string,
	displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	shaTag := ghclient.SHAMarker("merge-conflict", sha)
	alreadyPosted, postedAt, _ := p.gh.HasCommentContaining(ctx, pr.GetNumber(), shaTag)
	if alreadyPosted {
		lastContinue, _ := p.gh.LastMergeConflictContinueAt(ctx, pr.GetNumber())
		lastActivity := postedAt
		if lastContinue.After(lastActivity) {
			lastActivity = lastContinue
		}
		mergeTimeoutCfg := agentTimeoutCfg{
			countFn:      p.gh.CountMergeConflictContinueComments,
			nudgeMarker:  ghclient.MergeConflictContinueCommentMarker,
			promptKind:   "merge-conflict prompt",
			noticeFormat: "copilot-autocode: the Copilot coding agent became unresponsive while resolving merge conflicts and %d nudge(s) were exhausted. The PR has been left open in review for manual inspection.",
			statusVerb:   "nudges",
		}
		p.handleAgentTimeout(ctx, pr, num, lastActivity, mergeTimeoutCfg, displayInfo)
		displayInfo[num] = &issueDisplayInfo{
			current:     "Merge conflicts - waiting for Copilot",
			next:        "Re-checking next poll",
			pr:          pr,
			agentStatus: "pending",
		}
		return true, nil
	}

	mergePrompt := p.cfg.MergeConflictPrompt
	if !strings.Contains(mergePrompt, "@copilot") {
		mergePrompt = "@copilot " + mergePrompt
	}
	comment := mergePrompt + "\n" + ghclient.MergeConflictCommentMarker + "\n" + shaTag
	displayInfo[num] = &issueDisplayInfo{
		current:     "Merge conflicts detected",
		next:        "Asked Copilot to fix",
		pr:          pr,
		agentStatus: "pending",
	}
	return true, p.gh.PostComment(ctx, pr.GetNumber(), comment)
}

// processRefinementCycle manages the refinement round-trip for a review PR.
// It returns (done=true, nil) when all refinement rounds are exhausted and
// the caller should proceed to the merge step. It returns (done=false, nil)
// when waiting for the agent or just after posting a refinement prompt.
func (p *Poller) processRefinementCycle(
	ctx context.Context, pr *github.PullRequest, num int, sha string,
	sent int, anyFail bool, displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	if sent >= p.cfg.MaxRefinementRounds {
		return true, nil // all rounds used — proceed to merge
	}

	refSHATag := ghclient.SHAMarker("refinement", sha)
	alreadyPosted, postedAt, _ := p.gh.HasReviewContaining(ctx, pr.GetNumber(), refSHATag)
	if alreadyPosted {
		lastContinue, _ := p.gh.LastAgentContinueAt(ctx, pr.GetNumber())
		lastActivity := postedAt
		if lastContinue.After(lastActivity) {
			lastActivity = lastContinue
		}
		refinementTimeoutCfg := agentTimeoutCfg{
			countFn:      p.gh.CountAgentContinueComments,
			nudgeMarker:  ghclient.AgentContinueCommentMarker,
			promptKind:   "refinement prompt",
			noticeFormat: "copilot-autocode: the Copilot coding agent became unresponsive and %d continue attempt(s) were exhausted. The PR has been left open in review for manual inspection.",
			statusVerb:   "retries",
		}
		if p.handleAgentTimeout(ctx, pr, num, lastActivity, refinementTimeoutCfg, displayInfo) {
			// Agent is unresponsive to the refinement prompt.  Treat
			// refinement as done so the flow continues to the CI-fix
			// cycle (step 6.5), which posts a simpler, focused prompt.
			return true, nil
		}
		displayInfo[num] = &issueDisplayInfo{
			current:         fmt.Sprintf("Refinement %d/%d — waiting for agent", sent, p.cfg.MaxRefinementRounds),
			next:            "Waiting for agent to push",
			pr:              pr,
			refinementCount: sent,
			refinementMax:   p.cfg.MaxRefinementRounds,
			agentStatus:     "pending",
		}
		return false, nil
	}

	body := p.buildRefinementCIPrompt(ctx, sent+1, p.cfg.MaxRefinementRounds, num, anyFail, sha)
	if err := p.gh.PostReviewComment(ctx, pr.GetNumber(), body); err != nil {
		return false, err
	}
	sent++ // local increment for display
	displayInfo[num] = &issueDisplayInfo{
		current:         fmt.Sprintf("Refinement %d/%d posted — waiting for agent", sent, p.cfg.MaxRefinementRounds),
		next:            "Waiting for agent to push",
		pr:              pr,
		refinementCount: sent,
		refinementMax:   p.cfg.MaxRefinementRounds,
		agentStatus:     "pending",
	}
	return false, nil
}

// handleMissingPR is called by processOne when no open PR is found.
// It checks whether the PR was manually merged and closes the issue if so;
// otherwise it moves the issue back to ai-coding so the nudge flow re-triggers.
func (p *Poller) handleMissingPR(
	ctx context.Context, issue *github.Issue, num int,
	displayInfo map[int]*issueDisplayInfo,
) error {
	mergedPR, mErr := p.gh.MergedPRForIssue(ctx, issue)
	if mErr == nil && mergedPR != nil {
		logger.Load(ctx).InfoContext(ctx, "PR was manually merged; closing issue automatically",
			slog.Int("issue", num), slog.Int("pr", mergedPR.GetNumber()))
		displayInfo[num] = &issueDisplayInfo{
			current:     "PR merged manually - closing issue",
			pr:          mergedPR,
			agentStatus: "success",
		}
		for _, lbl := range []string{p.cfg.LabelReview, p.cfg.LabelCoding, p.cfg.LabelQueue} {
			_ = p.gh.RemoveLabel(ctx, num, lbl)
		}
		return p.gh.CloseIssue(ctx, num)
	}
	// No PR at all — move back to coding so the nudge flow re-triggers.
	logger.Load(ctx).InfoContext(ctx, "no open PR found; moving back to ai-coding", slog.Int("issue", num))
	displayInfo[num] = &issueDisplayInfo{
		current:     "No PR found - returning to coding",
		agentStatus: "failed",
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelReview, p.cfg.LabelCoding)
}

// processOne runs the unified refinement+CI feedback loop for a single
// ai-review issue.  Each round posts a combined prompt asking Copilot to both
// review its implementation against the original requirements AND fix any
// failing CI checks.  Rounds only advance once CI is clean for the current
// commit; the PR is merged after MaxRefinementRounds clean round-trips.
//
// Per-tick execution order:
//  1. Merge-conflict check — resolve first so CI can run on clean code.
//  2. Approve action_required workflow runs.
//  3. Wait for all runs to finish (return early if any are still active).
//  4. Check for agent coding timeout (timed_out run) → post continue if needed.
//  5. Refinement+CI cycle — post combined prompt for the current HEAD SHA.
//  6. Merge when refinementDone && allOK.
func (p *Poller) processOne(
	ctx context.Context, issue *github.Issue, displayInfo map[int]*issueDisplayInfo,
) error {
	num := issue.GetNumber()

	pr, err := p.gh.OpenPRForIssue(ctx, issue)
	if err != nil {
		return err
	}
	if pr == nil {
		return p.handleMissingPR(ctx, issue, num, displayInfo)
	}

	sha := pr.GetHead().GetSHA()

	// ── Step 1: Merge-conflict / behind-base ────────────────────────────────
	if handled, err := p.handleMergeConflict(ctx, pr, num, sha, displayInfo); err != nil {
		return err
	} else if handled {
		return nil
	}

	// ── Step 2: Approve action_required / waiting workflow runs ─────────────
	requiredRuns, pending, err := p.processRequiredRuns(ctx, pr, sha)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}

	// ── Step 2.5: Check for unapproved deployment gates ──────────────────
	if blocked := p.checkDeploymentGates(ctx, pr, num, sha, displayInfo); blocked {
		return nil
	}

	// ── Step 3: Wait for all workflow runs to finish ─────────────────────────
	anyActive, err := p.gh.AnyWorkflowRunActive(ctx, sha)
	if err != nil {
		return err
	}
	if anyActive || len(requiredRuns) > 0 {
		displayInfo[num] = &issueDisplayInfo{
			current:     "CI running — waiting for all checks",
			next:        "Waiting",
			pr:          pr,
			agentStatus: "pending",
		}
		return nil
	}

	// ── Step 4: Agent coding timeout ────────────────────────────────────────
	if handled, err := p.handleAgentFailure(ctx, pr, num, displayInfo); err != nil {
		return err
	} else if handled {
		return nil
	}

	// ── Step 5: Refinement + CI feedback cycle ───────────────────────────────
	allOK := p.cfg.SkipCIChecks
	var anyFail bool
	if !allOK {
		allOK, anyFail, err = p.gh.AllRunsSucceeded(ctx, sha)
		if err != nil {
			return err
		}
	}

	sent, err := p.gh.CountRefinementPromptsSent(ctx, pr.GetNumber())
	if err != nil {
		return err
	}

	done, err := p.processRefinementCycle(ctx, pr, num, sha, sent, anyFail, displayInfo)
	if err != nil {
		return err
	}
	if !done {
		return nil
	}

	// ── Step 6: Merge ────────────────────────────────────────────────────────
	if allOK {
		return p.mergeAndClose(ctx, pr, num, sent, displayInfo)
	}

	// ── Step 6.5: CI-fix-only cycle (post-refinement) ────────────────────────
	// Refinement rounds are exhausted but CI is still failing.  Keep posting
	// CI-fix prompts (no review requirements) up to MaxCIFixRounds.
	if anyFail {
		ciDone, ciErr := p.processCIFixCycle(ctx, pr, num, sha, displayInfo)
		if ciErr != nil {
			return ciErr
		}
		if !ciDone {
			return nil
		}
	}

	// CI-fix rounds also exhausted — report as stuck.
	displayInfo[num] = &issueDisplayInfo{
		current: fmt.Sprintf(
			"Refinements (%d/%d) and CI-fix rounds exhausted — CI still failing",
			sent, p.cfg.MaxRefinementRounds,
		),
		pr:              pr,
		refinementCount: sent,
		refinementMax:   p.cfg.MaxRefinementRounds,
		agentStatus:     "failed",
	}
	return nil
}

// processCIFixCycle handles the CI-fix-only loop that runs after refinement
// rounds are exhausted but CI is still failing.  It posts a prompt containing
// only the failing workflow/job details (no review requirements) so the agent
// can focus on fixing the tests.
//
// Returns (done=true, nil) when all CI-fix rounds are exhausted.
// Returns (done=false, nil) when waiting for the agent or just after posting.
func (p *Poller) processCIFixCycle(
	ctx context.Context, pr *github.PullRequest, num int, sha string,
	displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	ciFixSent, err := p.gh.CountCIFixPromptsSent(ctx, pr.GetNumber())
	if err != nil {
		return false, err
	}
	if ciFixSent >= p.cfg.MaxCIFixRounds {
		return true, nil // all CI-fix rounds used
	}

	ciSHATag := ghclient.SHAMarker("ci-fix", sha)
	alreadyPosted, postedAt, _ := p.gh.HasCommentContaining(ctx, pr.GetNumber(), ciSHATag)
	if alreadyPosted {
		lastContinue, _ := p.gh.LastAgentContinueAt(ctx, pr.GetNumber())
		lastActivity := postedAt
		if lastContinue.After(lastActivity) {
			lastActivity = lastContinue
		}
		ciFixTimeoutCfg := agentTimeoutCfg{
			countFn:      p.gh.CountAgentContinueComments,
			nudgeMarker:  ghclient.AgentContinueCommentMarker,
			promptKind:   "CI-fix prompt",
			noticeFormat: "copilot-autocode: the Copilot coding agent became unresponsive during CI fixing and %d continue attempt(s) were exhausted. The PR has been left open in review for manual inspection.",
			statusVerb:   "retries",
		}
		if p.handleAgentTimeout(ctx, pr, num, lastActivity, ciFixTimeoutCfg, displayInfo) {
			return false, nil
		}
		displayInfo[num] = &issueDisplayInfo{
			current:     fmt.Sprintf("CI-fix %d/%d — waiting for agent", ciFixSent, p.cfg.MaxCIFixRounds),
			next:        "Waiting for agent to push",
			pr:          pr,
			agentStatus: "pending",
		}
		return false, nil
	}

	// Build a CI-fix-only prompt (no review requirements).
	workflowName, failedJobs, err := p.gh.FailedRunDetails(ctx, sha)
	if err != nil {
		return false, err
	}
	body := fmt.Sprintf(
		"@copilot (CI-fix %d of %d). The tests are still failing — please fix them.%s\n%s\n%s",
		ciFixSent+1, p.cfg.MaxCIFixRounds,
		BuildCIFailureSection(workflowName, failedJobs),
		ghclient.CIFixCommentMarker,
		ciSHATag,
	)
	if err := p.gh.PostComment(ctx, pr.GetNumber(), body); err != nil {
		return false, err
	}
	ciFixSent++
	displayInfo[num] = &issueDisplayInfo{
		current:     fmt.Sprintf("CI-fix %d/%d posted — waiting for agent", ciFixSent, p.cfg.MaxCIFixRounds),
		next:        "Waiting for agent to push",
		pr:          pr,
		agentStatus: "pending",
	}
	return false, nil
}

// checkDeploymentGates checks for workflow runs that have concluded with
// action_required (environment deployment gates) which the orchestrator's
// token cannot approve.  When such runs exist, it posts a one-time notice
// and returns true so the caller skips the refinement/timeout flows.
func (p *Poller) checkDeploymentGates(
	ctx context.Context, pr *github.PullRequest, num int, sha string,
	displayInfo map[int]*issueDisplayInfo,
) bool {
	depRuns, err := p.gh.ListPendingDeploymentRuns(ctx, sha)
	if err != nil || len(depRuns) == 0 {
		return false
	}

	// Collect workflow names for the notice.
	var names []string
	for _, r := range depRuns {
		names = append(names, r.GetName())
	}

	displayInfo[num] = &issueDisplayInfo{
		current:     "Waiting for manual deployment approval",
		next:        fmt.Sprintf("Blocked by: %s", strings.Join(names, ", ")),
		pr:          pr,
		agentStatus: "pending",
	}

	// Post a one-time notice per SHA so we don't spam on every tick.
	shaTag := ghclient.SHAMarker("deployment-pending", sha)
	if posted, _, _ := p.gh.HasCommentContaining(ctx, pr.GetNumber(), shaTag); posted {
		return true
	}

	notice := fmt.Sprintf(
		"copilot-autocode: PR requires manual deployment approval for workflow(s): %s. "+
			"Waiting for a reviewer to approve the environment deployment before proceeding.\n%s\n%s",
		strings.Join(names, ", "),
		ghclient.DeploymentPendingCommentMarker,
		shaTag,
	)
	if err := p.gh.PostComment(ctx, pr.GetNumber(), notice); err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not post deployment-pending notice",
			slog.Int("pr", pr.GetNumber()), slog.Any("err", err))
	}
	return true
}

// processRequiredRuns approves any action_required / pending-deployment workflow
// runs on the PR.  It returns the approved run list, a done=true flag when the
// caller should exit early (retry limit hit), and any unexpected error.
func (p *Poller) processRequiredRuns(
	ctx context.Context, pr *github.PullRequest, sha string,
) ([]*github.WorkflowRun, bool, error) {
	// ── Fork-PR approval (status=action_required or status=waiting) ──────────
	runs, err := p.gh.ListActionRequiredRuns(ctx, sha)
	if err != nil {
		return nil, false, err
	}
	for _, r := range runs {
		runID := r.GetID()
		logger.Load(ctx).InfoContext(ctx, "approving workflow run",
			slog.Int("pr", pr.GetNumber()), slog.Int64("run", runID), slog.String("name", r.GetName()))
		if err := p.gh.ApproveWorkflowRun(ctx, runID); err != nil {
			logger.Load(ctx).WarnContext(ctx, "failed to approve workflow run",
				slog.Int64("run", runID), slog.Any("err", err))
			p.mu.Lock()
			p.approveRetries[runID]++
			count := p.approveRetries[runID]
			p.mu.Unlock()
			if count >= p.cfg.MaxAgentContinueRetries {
				logger.Load(ctx).InfoContext(ctx, "failed to approve workflow run after retries; leaving PR in review",
					slog.Int("pr", pr.GetNumber()), slog.Int64("run", runID), slog.Int("count", count))
				notice := fmt.Sprintf(
					"copilot-autocode: failed to automatically approve the GitHub Actions workflow run '%s' after %d retries. "+
						"The PR has been left open in review for manual inspection.",
					r.GetName(),
					count,
				)
				_ = p.gh.PostComment(ctx, pr.GetNumber(), notice)
				return nil, true, nil
			}
		} else {
			p.mu.Lock()
			delete(p.approveRetries, runID)
			p.mu.Unlock()
		}
	}

	// ── Environment deployment approval (status=completed conclusion=action_required) ──
	depRuns, err := p.gh.ListPendingDeploymentRuns(ctx, sha)
	if err != nil {
		return nil, false, err
	}
	for _, r := range depRuns {
		runID := r.GetID()
		logger.Load(ctx).InfoContext(ctx, "approving pending deployments for run",
			slog.Int("pr", pr.GetNumber()), slog.Int64("run", runID), slog.String("name", r.GetName()))
		approved, err := p.gh.ApprovePendingDeployments(ctx, runID)
		switch {
		case err != nil:
			logger.Load(ctx).WarnContext(ctx, "failed to approve pending deployments",
				slog.Int64("run", runID), slog.Any("err", err))
		case approved == 0:
			// No pending deployments — the action_required conclusion is
			// likely stale or from a GitHub App check suite.  Re-run the
			// workflow to clear the state and restart CI.
			logger.Load(ctx).InfoContext(ctx, "no pending deployments found; re-running workflow to clear action_required",
				slog.Int64("run", runID), slog.String("name", r.GetName()))
			if rerr := p.gh.RerunWorkflow(ctx, runID); rerr != nil {
				logger.Load(ctx).WarnContext(ctx, "failed to re-run workflow",
					slog.Int64("run", runID), slog.Any("err", rerr))
			} else {
				runs = append(runs, r)
			}
		default:
			logger.Load(ctx).InfoContext(ctx, "approved pending deployments",
				slog.Int64("run", runID), slog.Int("count", approved))
			runs = append(runs, r)
		}
	}

	return runs, false, nil
}

// mergeAndClose approves, merges, removes labels, and closes the issue once
// all CI checks pass and all refinement rounds are complete.
func (p *Poller) mergeAndClose(
	ctx context.Context, pr *github.PullRequest, num, sent int,
	displayInfo map[int]*issueDisplayInfo,
) error {
	displayInfo[num] = &issueDisplayInfo{
		current:         "All checks passed — merging",
		pr:              pr,
		refinementCount: sent,
		refinementMax:   p.cfg.MaxRefinementRounds,
		agentStatus:     "success",
	}
	logger.Load(ctx).InfoContext(ctx, "all checks passed and refinements done; merging",
		slog.Int("pr", pr.GetNumber()), slog.Int("refinements", sent))
	if err := p.gh.ApprovePR(ctx, pr.GetNumber()); err != nil {
		if !strings.Contains(err.Error(), "already approved") &&
			!strings.Contains(err.Error(), "Can not approve your own pull request") {
			return err
		}
	}
	if err := p.gh.MergePR(ctx, pr); err != nil {
		return err
	}
	for _, lbl := range []string{p.cfg.LabelReview, p.cfg.LabelCoding, p.cfg.LabelQueue} {
		_ = p.gh.RemoveLabel(ctx, num, lbl)
	}
	return p.gh.CloseIssue(ctx, num)
}

// handleAgentFailure checks whether the latest workflow run on the PR's HEAD
// SHA timed out and posts an "@copilot continue" comment to tell the agent to
// resume.  It waits AgentTimeoutRetryDelaySeconds before posting.
//
// Returns (true, nil) when the timeout was detected and handled (or is still
// within the delay window), so the caller should skip further processing.
// This includes the exhausted-retries case: when all continue attempts are
// consumed the issue is returned to the queue and (true, nil) is returned.
// Returns (false, nil) when no timed-out run was detected.
func (p *Poller) handleAgentFailure(
	ctx context.Context, pr *github.PullRequest,
	issueNum int, displayInfo map[int]*issueDisplayInfo,
) (bool, error) {
	sha := pr.GetHead().GetSHA()
	prNum := pr.GetNumber()

	// LatestFailedRunConclusion only returns a value for "timed_out" runs;
	// regular CI failures (conclusion="failure") are handled by the
	// refinement+CI feedback loop instead.
	conclusion, completedAt, err := p.gh.LatestFailedRunConclusion(ctx, sha)
	if err != nil {
		return false, err
	}
	if conclusion == "" {
		return false, nil // no timed-out run
	}

	// Count how many continue comments we've already posted.
	continueCount, err := p.gh.CountAgentContinueComments(ctx, prNum)
	if err != nil {
		return false, err
	}

	if continueCount >= p.cfg.MaxAgentContinueRetries {
		// All continue attempts exhausted — return the issue to the queue.
		logger.Load(ctx).InfoContext(ctx, "agent timed out; retries exhausted; returning to queue",
			slog.Int("pr", prNum), slog.Int("attempts", continueCount), slog.Int("issue", issueNum))
		notice := fmt.Sprintf(
			"copilot-autocode: the Copilot coding agent timed out and %d continue "+
				"attempt(s) were exhausted. The PR has been left open in review for manual inspection.",
			continueCount,
		)
		_ = p.gh.PostComment(ctx, issueNum, notice)
		displayInfo[issueNum] = &issueDisplayInfo{
			current:     fmt.Sprintf("Agent timed out after %d retries — left in review", continueCount),
			pr:          pr,
			agentStatus: "failed",
		}
		return true, nil
	}

	// Wait the configured delay before posting continue, using the later of
	// the run completion time and the last continue comment.
	delay := time.Duration(p.cfg.AgentTimeoutRetryDelaySeconds) * time.Second
	lastContinue, err := p.gh.LastAgentContinueAt(ctx, prNum)
	if err != nil {
		return false, err
	}
	lastActivity := completedAt
	if lastContinue.After(lastActivity) {
		lastActivity = lastContinue
	}
	// Guard against zero-value timestamps that would cause indefinite waits
	// (deadline far in the past relative to time.Now).
	if lastActivity.IsZero() {
		lastActivity = time.Now()
	}
	deadline := lastActivity.Add(delay)
	if time.Now().Before(deadline) {
		displayInfo[issueNum] = &issueDisplayInfo{
			current: fmt.Sprintf(
				"Agent timed out (attempt %d of %d)",
				continueCount+1, p.cfg.MaxAgentContinueRetries,
			),
			next:         "Post continue",
			nextActionAt: deadline,
			pr:           pr,
			agentStatus:  "failed",
		}
		return true, nil
	}

	logger.Load(ctx).InfoContext(ctx, "agent timed out; posting continue",
		slog.Int("pr", prNum), slog.Int("attempt", continueCount+1), slog.Int("max", p.cfg.MaxAgentContinueRetries))
	displayInfo[issueNum] = &issueDisplayInfo{
		current: fmt.Sprintf(
			"Agent timed out — posting continue (%d of %d)",
			continueCount+1, p.cfg.MaxAgentContinueRetries,
		),
		next:        "Waiting for agent",
		pr:          pr,
		agentStatus: "pending",
	}
	comment := fmt.Sprintf(
		"%s\n\ncopilot-autocode: agent run timed out (attempt %d of %d).\n%s",
		p.cfg.AgentContinuePrompt,
		continueCount+1, p.cfg.MaxAgentContinueRetries,
		ghclient.AgentContinueCommentMarker,
	)
	if err := p.gh.PostComment(ctx, prNum, comment); err != nil {
		return false, err
	}
	return true, nil
}

// buildRefinementCIPrompt composes the combined review+CI-fix comment posted
// each refinement round.  It always asks Copilot to review its implementation
// against the original issue requirements.  When CI is currently failing it
// also appends the failing workflow name, job names, and per-job log URLs so
// Copilot can fix code and tests in the same pass — eliminating the separate
// CI-fix round trip.
func (p *Poller) buildRefinementCIPrompt(
	ctx context.Context, round, maxRounds, issueNum int, anyFail bool, sha string,
) string {
	var sb strings.Builder

	fmt.Fprintf(&sb,
		"@copilot (refinement check %d of %d against issue #%d). %s "+
			"Please address any review feedback if not already addressed.",
		round, maxRounds, issueNum, p.cfg.RefinementPrompt,
	)

	if anyFail {
		workflowName, failedJobs, err := p.gh.FailedRunDetails(ctx, sha)
		if err == nil {
			sb.WriteString(BuildCIFailureSection(workflowName, failedJobs))
		}
	}

	sb.WriteString("\n" + ghclient.RefinementCommentMarker)
	sb.WriteString("\n" + ghclient.SHAMarker("refinement", sha))
	return sb.String()
}

// BuildCIFailureSection formats the CI-failure block appended to refinement
// prompts when CI is currently failing.  Extracted so it can be unit-tested
// without a network mock.
func BuildCIFailureSection(workflowName string, failedJobs []ghclient.FailedJobInfo) string {
	var sb strings.Builder
	sb.WriteString("\n\n**Additionally, please fix the following CI failures before pushing:**")
	if workflowName != "" {
		fmt.Fprintf(&sb, "\n**Failing workflow:** %s", workflowName)
	}
	if len(failedJobs) > 0 {
		names := make([]string, len(failedJobs))
		for i, j := range failedJobs {
			names[i] = j.Name
		}
		fmt.Fprintf(&sb, "\n**Failed jobs:** %s", strings.Join(names, ", "))
		for _, job := range failedJobs {
			if job.LogURL != "" {
				fmt.Fprintf(&sb, "\n\n**%s** logs: %s", job.Name, job.LogURL)
			}
		}
	}
	return sb.String()
}

// snapshot builds the current state for the TUI, enriching each State with
// the human-readable status computed by the processing steps above.
// PR references are read from the displayInfo cache populated during
// processing, avoiding redundant OpenPRForIssue API calls.
func (p *Poller) snapshot(ctx context.Context, displayInfo map[int]*issueDisplayInfo) ([]*State, []*State, []*State) {
	queueIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	codingIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	reviewIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)

	// Sort before building states so the output order is deterministic.
	SortIssuesAsc(queueIssues)
	SortIssuesAsc(codingIssues)
	SortIssuesAsc(reviewIssues)

	toStates := func(issues []*github.Issue, status string) []*State {
		states := make([]*State, 0, len(issues))
		for _, i := range issues {
			s := &State{Issue: i, Status: status}
			if status == "queue" {
				s.CurrentStatus = "Waiting to be assigned"
				s.NextAction = "Assign Copilot"
			} else if info, ok := displayInfo[i.GetNumber()]; ok {
				s.CurrentStatus = info.current
				s.NextAction = info.next
				s.NextActionAt = info.nextActionAt
				s.PR = info.pr
				s.RefinementCount = info.refinementCount
				s.RefinementMax = info.refinementMax
				s.AgentStatus = info.agentStatus
			}
			states = append(states, s)
		}
		return states
	}

	return toStates(queueIssues, "queue"),
		toStates(codingIssues, "coding"),
		toStates(reviewIssues, "review")
}

// -- helpers -----------------------------------------------------------------

// deduplicateIssueLists removes issues that appear in multiple phase lists
// so each issue is processed only once, in its most-advanced phase.
// Priority: reviewing > coding > queue.  The queue list is not modified
// (promoteFromQueue already checks for existing PRs), but issues in queue
// are removed from coding, and issues in reviewing are removed from coding.
func deduplicateIssueLists(queue, coding, reviewing []*github.Issue) ([]*github.Issue, []*github.Issue) {
	seen := make(map[int]struct{}, len(reviewing)+len(queue))
	for _, i := range reviewing {
		seen[i.GetNumber()] = struct{}{}
	}
	// Remove from coding any issue already in reviewing.
	filtered := coding[:0]
	for _, i := range coding {
		if _, dup := seen[i.GetNumber()]; !dup {
			filtered = append(filtered, i)
		} else {
			// No context available here, just using slog.Default() for these non-critical background logs
			slog.Default().Warn("issue appears in both coding and review lists; processing only in review",
				slog.Int("issue", i.GetNumber()))
		}
	}
	// Also remove from coding any issue still in queue (shouldn't happen
	// but guards against partial label swaps).
	queueSet := make(map[int]struct{}, len(queue))
	for _, i := range queue {
		queueSet[i.GetNumber()] = struct{}{}
	}
	deduped := filtered[:0]
	for _, i := range filtered {
		if _, dup := queueSet[i.GetNumber()]; !dup {
			deduped = append(deduped, i)
		} else {
			// No context available here
			slog.Default().Warn("issue appears in both queue and coding lists; processing only in queue",
				slog.Int("issue", i.GetNumber()))
		}
	}
	return deduped, reviewing
}

func SortIssuesAsc(issues []*github.Issue) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].GetNumber() < issues[j-1].GetNumber(); j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}

// FormatFallbackPrompt expands the well-known placeholders in the configured
// FallbackIssueInvokePrompt with live data from the given issue:
//
//	{issue_number} → issue number (e.g. "42")
//	{issue_title}  → issue title
//	{issue_url}    → HTML URL of the issue on GitHub
func FormatFallbackPrompt(template string, issue *github.Issue) string {
	r := strings.NewReplacer(
		"{issue_number}", strconv.Itoa(issue.GetNumber()),
		"{issue_title}", issue.GetTitle(),
		"{issue_url}", issue.GetHTMLURL(),
	)
	return r.Replace(template)
}

func (p *Poller) handleNudgeExhaustion(
	ctx context.Context, num, nudgeCount int,
	displayInfo map[int]*issueDisplayInfo,
) error {
	logger.Load(ctx).InfoContext(ctx, "Copilot did not start after multiple nudges; returning to queue",
		slog.Int("issue", num), slog.Int("nudges", nudgeCount))

	displayInfo[num] = &issueDisplayInfo{
		current:     fmt.Sprintf("No response after %d nudge(s) — returning to queue", nudgeCount),
		agentStatus: "failed",
	}

	notice := fmt.Sprintf(
		"copilot-autocode: Copilot has not started after %d nudge attempt(s). "+
			"Returning this issue to the queue for manual review. "+
			"Check that the GitHub Copilot coding agent is enabled for this repository.",
		nudgeCount,
	)
	if err := p.gh.PostComment(ctx, num, notice); err != nil {
		logger.Load(ctx).ErrorContext(ctx, "failed to post exhaustion notice",
			slog.Int("issue", num), slog.Any("err", err))
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelQueue)
}
