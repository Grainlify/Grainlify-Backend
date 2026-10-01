package handlers

import (
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

// Maintainer-funded bounties: the relay.
//
// Nothing here holds a key or moves money. The funder signs every transaction
// that touches their escrow in their own wallet; the agent builds them and
// reads the chain afterwards. What this service adds is what only it knows:
// who is signed in, and whether a repository is a verified Grainlify project.
//
// Three channels, as everywhere else in the bounty relay:
//   - the funder's actions on the maintainer channel. Signed for anybody
//     signed in; the agent checks, per bounty, that they FUNDED it.
//   - the contributor's answer to a proposal on the apply channel, which is
//     theirs; the agent checks it is their assignment by GitHub id.
//   - disputes on the admin channel, only behind requireAdmin (see
//     internal/api TestAdminActionsAreAdminOnly).

var fullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// verifiedProject answers the question the agent cannot: is this repository a
// project registered on Grainlify, verified, with our GitHub App installed?
// The same test as PostBountyRepo, deliberately.
func (h *BountyDrawHandler) verifiedProject(c *fiber.Ctx, fullName string) bool {
	var verified bool
	var installation *string
	err := h.db.Pool.QueryRow(c.Context(), `
SELECT (verified_at IS NOT NULL), github_app_installation_id
  FROM projects WHERE lower(github_full_name) = lower($1) AND deleted_at IS NULL
 LIMIT 1`, fullName).Scan(&verified, &installation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("funded bounty: could not read the project", "full_name", fullName, "error", err)
	}
	return err == nil && verified && installation != nil && *installation != ""
}

// MaintainerFundedPrepare builds the funding transaction for a new funded bounty.
//
// Refused here, before anything is signed, when the repository is not a
// verified project: the agent would refuse too, but the funder should hear it
// from the service that knows why.
func (h *BountyDrawHandler) MaintainerFundedPrepare(c *fiber.Ctx) error {
	var in struct {
		Repo         string `json:"repo"`
		IssueNumber  int    `json:"issue_number"`
		AmountMinor  string `json:"amount_minor"`
		Currency     string `json:"currency"`
		Mode         string `json:"mode"`
		Deadline     string `json:"deadline"`
		FunderWallet string `json:"funder_wallet"`
	}
	if err := c.BodyParser(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	repo := strings.TrimSpace(in.Repo)
	if !fullNameRe.MatchString(repo) || in.IssueNumber <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_issue", "detail": "name the repository as owner/name and an issue number"})
	}
	if in.Mode != "draw" && in.Mode != "self_assign" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_mode", "detail": "choose the draw or assigning it yourself"})
	}
	if !h.verifiedProject(c, repo) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_a_verified_project",
			"detail": repo + " is not a verified Grainlify project with the GitHub App installed. Register and verify it first."})
	}
	return h.maintainerAction(c, "funded_prepare", repo+":"+strconv.Itoa(in.IssueNumber), map[string]any{
		"amountMinor":     in.AmountMinor,
		"currency":        in.Currency,
		"mode":            in.Mode,
		"deadline":        in.Deadline,
		"funderWallet":    in.FunderWallet,
		"verifiedProject": true,
	})
}

// MaintainerFundedView is the funder's view of their bounty.
func (h *BountyDrawHandler) MaintainerFundedView(c *fiber.Ctx) error {
	return h.maintainerAction(c, "funded_view", c.Params("bountyId"), nil)
}

type fundedBody struct {
	Signature string `json:"signature"`
	Applicant string `json:"applicant"`
	Reason    string `json:"reason"`
	Simulate  bool   `json:"simulate"`
}

func fundedInput(c *fiber.Ctx) fundedBody {
	var in fundedBody
	_ = c.BodyParser(&in) // every field is optional somewhere; the agent says which are required
	return in
}

// MaintainerFundedConfirm: the funder signed the funding; the agent reads the chain.
func (h *BountyDrawHandler) MaintainerFundedConfirm(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_confirm", c.Params("bountyId"), map[string]any{"signature": in.Signature})
}

// MaintainerFundedAssign builds the self-assign transaction naming one applicant.
func (h *BountyDrawHandler) MaintainerFundedAssign(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_assign_prepare", c.Params("bountyId"), map[string]any{"applicant": in.Applicant})
}

// MaintainerFundedAssignConfirm records a self-assignment once the chain shows it.
func (h *BountyDrawHandler) MaintainerFundedAssignConfirm(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_assign_confirm", c.Params("bountyId"),
		map[string]any{"applicant": in.Applicant, "signature": in.Signature})
}

// MaintainerFundedDraw runs the draw on a draw-mode funded bounty.
func (h *BountyDrawHandler) MaintainerFundedDraw(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_draw", c.Params("bountyId"), map[string]any{"simulate": in.Simulate})
}

// MaintainerFundedUnassign ends an assignment: freely before a pull request,
// only once agreed after one. In self-assign mode the answer is a transaction
// for the funder to sign.
func (h *BountyDrawHandler) MaintainerFundedUnassign(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_unassign", c.Params("bountyId"), map[string]any{"reason": in.Reason})
}

// MaintainerFundedUnassignConfirm records a self-assign unassignment once the chain shows it.
func (h *BountyDrawHandler) MaintainerFundedUnassignConfirm(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_unassign_confirm", c.Params("bountyId"),
		map[string]any{"reason": in.Reason, "signature": in.Signature})
}

// MaintainerFundedPropose proposes unassigning once a pull request is open.
func (h *BountyDrawHandler) MaintainerFundedPropose(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.maintainerAction(c, "funded_propose", c.Params("bountyId"), map[string]any{"reason": in.Reason})
}

// MaintainerFundedAnswer is the funder accepting, refusing or withdrawing a proposal.
func (h *BountyDrawHandler) MaintainerFundedAnswer(c *fiber.Ctx) error {
	action, ok := map[string]string{"accept": "funded_accept", "refuse": "funded_refuse", "withdraw": "funded_withdraw"}[c.Params("answer")]
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
	}
	in := fundedInput(c)
	return h.maintainerAction(c, action, c.Params("proposalId"), map[string]any{"reason": in.Reason})
}

// contributorAction relays on the apply channel: the contributor acting on
// their own assignment. The agent matches them to it by GitHub id.
func (h *BountyDrawHandler) contributorAction(c *fiber.Ctx, action, subject string, extra map[string]any) error {
	id, login, err := h.githubFor(c)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "github_not_linked"})
	}
	if err != nil {
		if err.Error() == "unauthenticated" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return h.call(c, bountyApplyDomain, "apply", action, id, login, subject, extra)
}

// PostUnassignPropose is the contributor proposing to give up an assignment
// whose pull request is open.
func (h *BountyDrawHandler) PostUnassignPropose(c *fiber.Ctx) error {
	in := fundedInput(c)
	return h.contributorAction(c, "unassign_propose", c.Params("bountyId"), map[string]any{"text": in.Reason})
}

// PostUnassignAnswer is the contributor accepting, refusing or withdrawing.
func (h *BountyDrawHandler) PostUnassignAnswer(c *fiber.Ctx) error {
	action, ok := map[string]string{"accept": "unassign_accept", "refuse": "unassign_refuse", "withdraw": "unassign_withdraw"}[c.Params("answer")]
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
	}
	in := fundedInput(c)
	return h.contributorAction(c, action, c.Params("proposalId"), map[string]any{"text": in.Reason})
}

// GetBountyDisputes lists refused proposals: the only cases an admin is asked to look at.
func (h *BountyDrawHandler) GetBountyDisputes(c *fiber.Ctx) error {
	return h.adminAction(c, "dispute_list", "", nil)
}

// GetBountyDispute is one dispute with its timeline and both positions.
func (h *BountyDrawHandler) GetBountyDispute(c *fiber.Ctx) error {
	return h.adminAction(c, "dispute_view", c.Params("id"), nil)
}

// PostBountyDisputeLeave records "leave it to the deadline". There is no
// action that ends a dispute early in the funder's favour.
func (h *BountyDrawHandler) PostBountyDisputeLeave(c *fiber.Ctx) error {
	return h.adminAction(c, "dispute_leave", c.Params("id"), nil)
}

// PostBountyDisputeNote records a conduct note on the funder.
func (h *BountyDrawHandler) PostBountyDisputeNote(c *fiber.Ctx) error {
	var in struct {
		Note string `json:"note"`
	}
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Note) == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "note_required"})
	}
	return h.adminAction(c, "dispute_note", c.Params("id"), map[string]any{"note": in.Note})
}
