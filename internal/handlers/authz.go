package handlers

import (
	"context"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ownerOrLiveAdmin reports whether userID may act on something owned by owner.
//
// # Why this exists
//
// /admin/* has been authorised against the database since RequireLiveRole
// landed: 50 routes, no exemptions, no cache, one read per request. Next to it,
// every owner-or-admin decision on ordinary project routes read the JWT's role
// claim - which is issued once, lives an hour, and cannot be revoked. So a
// demoted admin lost the admin surface on their next request and kept the
// ability to write to other people's projects until their token expired.
//
// The claim is not authorisation. It is a copy of a fact, taken at login.
//
// # Ownership first, and why that is not just an optimisation
//
// Owners never trigger a role read at all. That is the common path, so this
// adds nothing to it - and it is worth being exact rather than saying "the
// role read joins onto the ownership query", because a join would read the
// role even when ownership had already decided the answer.
//
// The non-owner path costs exactly one indexed primary-key read. That is the
// same trade RequireLiveRole documents and refuses to cache away: a cache
// would make revocation eventual again, which is the property being bought.
// Anyone adding one here has quietly reverted this change.
//
// # Fails closed
//
// A lookup error refuses. "We could not check the role" must never read as
// "the role is admin" - the same rule RequireLiveRole and the escrow
// verification follow, and for the same reason: an unverified claim is not a
// verified one.
func ownerOrLiveAdmin(ctx context.Context, d *db.DB, owner, userID uuid.UUID) (bool, error) {
	if owner == userID {
		return true, nil
	}
	role, err := NewRoleLookup(d)(ctx, userID)
	if err != nil {
		return false, err
	}
	return role == "admin", nil
}
