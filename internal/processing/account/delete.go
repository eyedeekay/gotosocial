// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package account

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"codeberg.org/gruf/go-kv"
	"github.com/google/uuid"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
	"golang.org/x/crypto/bcrypt"
)

const deleteSelectLimit = 50

// Delete deletes an account, and all of that account's statuses, media, follows, notifications, etc etc etc.
// The origin passed here should be either the ID of the account doing the delete (can be itself), or the ID of a domain block.
func (p *Processor) Delete(ctx context.Context, account *gtsmodel.Account, origin string) gtserror.WithCode {
	l := log.WithContext(ctx).WithFields(kv.Fields{
		{"username", account.Username},
		{"domain", account.Domain},
	}...)
	l.Trace("beginning account delete process")

	if account.IsLocal() {
		if err := p.deleteUserAndTokensForAccount(ctx, account); err != nil {
			return gtserror.NewErrorInternalError(err)
		}
	}

	if err := p.deleteAccountFollows(ctx, account); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	if err := p.deleteAccountBlocks(ctx, account); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	if err := p.deleteAccountStatuses(ctx, account); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	if err := p.deleteAccountNotifications(ctx, account); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	if err := p.deleteAccountPeripheral(ctx, account); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	// To prevent the account being created again,
	// stubbify it and update it in the db.
	// The account will not be deleted, but it
	// will become completely unusable.
	columns := stubbifyAccount(account, origin)
	if err := p.state.DB.UpdateAccount(ctx, account, columns...); err != nil {
		return gtserror.NewErrorInternalError(err)
	}

	l.Info("account deleted")
	return nil
}

// DeleteSelf is like Delete, but specifically for local accounts deleting themselves.
//
// Calling DeleteSelf results in a delete message being enqueued in the processor,
// which causes side effects to occur: delete will be federated out to other instances,
// and the above Delete function will be called afterwards from the processor, to clear
// out the account's bits and bobs, and stubbify it.
func (p *Processor) DeleteSelf(ctx context.Context, account *gtsmodel.Account) gtserror.WithCode {
	fromClientAPIMessage := messages.FromClientAPI{
		APObjectType:   ap.ActorPerson,
		APActivityType: ap.ActivityDelete,
		OriginAccount:  account,
		TargetAccount:  account,
	}

	// Process the delete side effects asynchronously.
	p.state.Workers.EnqueueClientAPI(ctx, fromClientAPIMessage)

	return nil
}

// deleteUserAndTokensForAccount deletes the gtsmodel.User and
// any OAuth tokens and applications for the given account.
//
// Callers to this function should already have checked that
// this is a local account, or else it won't have a user associated
// with it, and this will fail.
func (p *Processor) deleteUserAndTokensForAccount(ctx context.Context, account *gtsmodel.Account) error {
	user, err := p.state.DB.GetUserByAccountID(ctx, account.ID)
	if err != nil {
		return fmt.Errorf("deleteUserAndTokensForAccount: db error getting user: %w", err)
	}

	tokens := []*gtsmodel.Token{}
	if err := p.state.DB.GetWhere(ctx, []db.Where{{Key: "user_id", Value: user.ID}}, &tokens); err != nil {
		return fmt.Errorf("deleteUserAndTokensForAccount: db error getting tokens: %w", err)
	}

	for _, t := range tokens {
		// Delete any OAuth clients associated with this token.
		if err := p.state.DB.DeleteByID(ctx, t.ClientID, &[]*gtsmodel.Client{}); err != nil {
			return fmt.Errorf("deleteUserAndTokensForAccount: db error deleting client: %w", err)
		}

		// Delete any OAuth applications associated with this token.
		if err := p.state.DB.DeleteWhere(ctx, []db.Where{{Key: "client_id", Value: t.ClientID}}, &[]*gtsmodel.Application{}); err != nil {
			return fmt.Errorf("deleteUserAndTokensForAccount: db error deleting application: %w", err)
		}

		// Delete the token itself.
		if err := p.state.DB.DeleteByID(ctx, t.ID, t); err != nil {
			return fmt.Errorf("deleteUserAndTokensForAccount: db error deleting token: %w", err)
		}
	}

	columns, err := stubbifyUser(user)
	if err != nil {
		return fmt.Errorf("deleteUserAndTokensForAccount: error stubbifying user: %w", err)
	}

	if err := p.state.DB.UpdateUser(ctx, user, columns...); err != nil {
		return fmt.Errorf("deleteUserAndTokensForAccount: db error updating user: %w", err)
	}

	return nil
}

// deleteAccountFollows deletes:
//   - Follows targeting account.
//   - Follow requests targeting account.
//   - Follows created by account.
//   - Follow requests created by account.
func (p *Processor) deleteAccountFollows(ctx context.Context, account *gtsmodel.Account) error {
	// Delete follows targeting this account.
	followedBy, err := p.state.DB.GetAccountFollowers(ctx, account.ID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return fmt.Errorf("deleteAccountFollows: db error getting follows targeting account %s: %w", account.ID, err)
	}

	for _, follow := range followedBy {
		if err := p.state.DB.DeleteFollowByID(ctx, follow.ID); err != nil {
			return fmt.Errorf("deleteAccountFollows: db error unfollowing account followedBy: %w", err)
		}
	}

	// Delete follow requests targeting this account.
	followRequestedBy, err := p.state.DB.GetAccountFollowRequests(ctx, account.ID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return fmt.Errorf("deleteAccountFollows: db error getting follow requests targeting account %s: %w", account.ID, err)
	}

	for _, followRequest := range followRequestedBy {
		if err := p.state.DB.DeleteFollowRequestByID(ctx, followRequest.ID); err != nil {
			return fmt.Errorf("deleteAccountFollows: db error unfollowing account followRequestedBy: %w", err)
		}
	}

	var (
		// Use this slice to batch unfollow messages.
		msgs = []messages.FromClientAPI{}
		// To avoid checking if account is local over + over
		// inside the subsequent loops, just generate static
		// side effects function once now.
		unfollowSideEffects = p.unfollowSideEffectsFunc(account)
	)

	// Delete follows originating from this account.
	following, err := p.state.DB.GetAccountFollows(ctx, account.ID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return fmt.Errorf("deleteAccountFollows: db error getting follows owned by account %s: %w", account.ID, err)
	}

	// For each follow owned by this account, unfollow
	// and process side effects (noop if remote account).
	for _, follow := range following {
		if err := p.state.DB.DeleteFollowByID(ctx, follow.ID); err != nil {
			return fmt.Errorf("deleteAccountFollows: db error unfollowing account: %w", err)
		}
		if msg := unfollowSideEffects(ctx, account, follow); msg != nil {
			// There was a side effect to process.
			msgs = append(msgs, *msg)
		}
	}

	// Delete follow requests originating from this account.
	followRequesting, err := p.state.DB.GetAccountFollowRequesting(ctx, account.ID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return fmt.Errorf("deleteAccountFollows: db error getting follow requests owned by account %s: %w", account.ID, err)
	}

	// For each follow owned by this account, unfollow
	// and process side effects (noop if remote account).
	for _, followRequest := range followRequesting {
		if err := p.state.DB.DeleteFollowRequestByID(ctx, followRequest.ID); err != nil {
			return fmt.Errorf("deleteAccountFollows: db error unfollowingRequesting account: %w", err)
		}

		// Dummy out a follow so our side effects func
		// has something to work with. This follow will
		// never enter the db, it's just for convenience.
		follow := &gtsmodel.Follow{
			URI:             followRequest.URI,
			AccountID:       followRequest.AccountID,
			Account:         followRequest.Account,
			TargetAccountID: followRequest.TargetAccountID,
			TargetAccount:   followRequest.TargetAccount,
		}

		if msg := unfollowSideEffects(ctx, account, follow); msg != nil {
			// There was a side effect to process.
			msgs = append(msgs, *msg)
		}
	}

	// Process accreted messages asynchronously.
	p.state.Workers.EnqueueClientAPI(ctx, msgs...)

	return nil
}

func (p *Processor) unfollowSideEffectsFunc(deletedAccount *gtsmodel.Account) func(ctx context.Context, account *gtsmodel.Account, follow *gtsmodel.Follow) *messages.FromClientAPI {
	if !deletedAccount.IsLocal() {
		// Don't try to process side effects
		// for accounts that aren't local.
		return func(ctx context.Context, account *gtsmodel.Account, follow *gtsmodel.Follow) *messages.FromClientAPI {
			return nil // noop
		}
	}

	return func(ctx context.Context, account *gtsmodel.Account, follow *gtsmodel.Follow) *messages.FromClientAPI {
		if follow.TargetAccount == nil {
			// TargetAccount seems to have gone;
			// race condition? db corruption?
			log.WithContext(ctx).WithField("follow", follow).Warn("follow had no TargetAccount, likely race condition")
			return nil
		}

		if follow.TargetAccount.IsLocal() {
			// No side effects for local unfollows.
			return nil
		}

		// There was a follow, process side effects.
		return &messages.FromClientAPI{
			APObjectType:   ap.ActivityFollow,
			APActivityType: ap.ActivityUndo,
			GTSModel:       follow,
			OriginAccount:  account,
			TargetAccount:  follow.TargetAccount,
		}
	}
}

func (p *Processor) deleteAccountBlocks(ctx context.Context, account *gtsmodel.Account) error {
	if err := p.state.DB.DeleteAccountBlocks(ctx, account.ID); err != nil {
		return fmt.Errorf("deleteAccountBlocks: db error deleting account blocks for %s: %w", account.ID, err)
	}
	return nil
}

// deleteAccountStatuses iterates through all statuses owned by
// the given account, passing each discovered status (and boosts
// thereof) to the processor workers for further async processing.
func (p *Processor) deleteAccountStatuses(ctx context.Context, account *gtsmodel.Account) error {
	// We'll select statuses 50 at a time so we don't wreck the db,
	// and pass them through to the client api worker to handle.
	//
	// Deleting the statuses in this way also handles deleting the
	// account's media attachments, mentions, and polls, since these
	// are all attached to statuses.

	var (
		statuses []*gtsmodel.Status
		err      error
		maxID    string
		msgs     = []messages.FromClientAPI{}
	)

statusLoop:
	for {
		// Page through account's statuses.
		statuses, err = p.state.DB.GetAccountStatuses(ctx, account.ID, deleteSelectLimit, false, false, maxID, "", false, false)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			// Make sure we don't have a real error.
			return err
		}

		if len(statuses) == 0 {
			break statusLoop
		}

		// Update next maxID from last status.
		maxID = statuses[len(statuses)-1].ID

		for _, status := range statuses {
			status.Account = account // ensure account is set

			// Pass the status delete through the client api worker for processing.
			msgs = append(msgs, messages.FromClientAPI{
				APObjectType:   ap.ObjectNote,
				APActivityType: ap.ActivityDelete,
				GTSModel:       status,
				OriginAccount:  account,
				TargetAccount:  account,
			})

			// Look for any boosts of this status in DB.
			boosts, err := p.state.DB.GetStatusReblogs(ctx, status)
			if err != nil && !errors.Is(err, db.ErrNoEntries) {
				return fmt.Errorf("deleteAccountStatuses: error fetching status reblogs for %s: %w", status.ID, err)
			}

			for _, boost := range boosts {
				if boost.Account == nil {
					// Fetch the relevant account for this status boost.
					boostAcc, err := p.state.DB.GetAccountByID(ctx, boost.AccountID)
					if err != nil {
						if errors.Is(err, db.ErrNoEntries) {
							// We don't have an account for this boost
							// for some reason, so just skip processing.
							log.WithContext(ctx).WithField("boost", boost).Warnf("no account found with id %s for boost %s", boost.AccountID, boost.ID)
							continue
						}
						return fmt.Errorf("deleteAccountStatuses: error fetching boosted status account for %s: %w", boost.AccountID, err)
					}

					// Set account model
					boost.Account = boostAcc
				}

				// Pass the boost delete through the client api worker for processing.
				msgs = append(msgs, messages.FromClientAPI{
					APObjectType:   ap.ActivityAnnounce,
					APActivityType: ap.ActivityUndo,
					GTSModel:       status,
					OriginAccount:  boost.Account,
					TargetAccount:  account,
				})
			}
		}
	}

	// Batch process all accreted messages.
	p.state.Workers.EnqueueClientAPI(ctx, msgs...)

	return nil
}

func (p *Processor) deleteAccountNotifications(ctx context.Context, account *gtsmodel.Account) error {
	// Delete all notifications of all types targeting given account.
	if err := p.state.DB.DeleteNotifications(ctx, nil, account.ID, ""); err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	// Delete all notifications of all types originating from given account.
	if err := p.state.DB.DeleteNotifications(ctx, nil, "", account.ID); err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	return nil
}

func (p *Processor) deleteAccountPeripheral(ctx context.Context, account *gtsmodel.Account) error {
	// Delete all bookmarks owned by given account.
	if err := p.state.DB.DeleteStatusBookmarks(ctx, account.ID, ""); // nocollapse
	err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	// Delete all bookmarks targeting given account.
	if err := p.state.DB.DeleteStatusBookmarks(ctx, "", account.ID); // nocollapse
	err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	// Delete all faves owned by given account.
	if err := p.state.DB.DeleteStatusFaves(ctx, account.ID, ""); // nocollapse
	err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	// Delete all faves targeting given account.
	if err := p.state.DB.DeleteStatusFaves(ctx, "", account.ID); // nocollapse
	err != nil && !errors.Is(err, db.ErrNoEntries) {
		return err
	}

	// TODO: add status mutes here when they're implemented.

	return nil
}

// stubbifyAccount renders the given account as a stub,
// removing most information from it and marking it as
// suspended.
//
// The origin parameter refers to the origin of the
// suspension action; should be an account ID or domain
// block ID.
//
// For caller's convenience, this function returns the db
// names of all columns that are updated by it.
func stubbifyAccount(account *gtsmodel.Account, origin string) []string {
	var (
		falseBool = func() *bool { b := false; return &b }
		trueBool  = func() *bool { b := true; return &b }
		now       = time.Now()
		never     = time.Time{}
	)

	account.FetchedAt = never
	account.AvatarMediaAttachmentID = ""
	account.AvatarRemoteURL = ""
	account.HeaderMediaAttachmentID = ""
	account.HeaderRemoteURL = ""
	account.DisplayName = ""
	account.EmojiIDs = nil
	account.Emojis = nil
	account.Fields = nil
	account.Note = ""
	account.NoteRaw = ""
	account.Memorial = falseBool()
	account.AlsoKnownAs = ""
	account.MovedToAccountID = ""
	account.Reason = ""
	account.Discoverable = falseBool()
	account.StatusContentType = ""
	account.CustomCSS = ""
	account.SuspendedAt = now
	account.SuspensionOrigin = origin
	account.HideCollections = trueBool()
	account.EnableRSS = falseBool()

	return []string{
		"fetched_at",
		"avatar_media_attachment_id",
		"avatar_remote_url",
		"header_media_attachment_id",
		"header_remote_url",
		"display_name",
		"emojis",
		"fields",
		"note",
		"note_raw",
		"memorial",
		"also_known_as",
		"moved_to_account_id",
		"reason",
		"discoverable",
		"status_content_type",
		"custom_css",
		"suspended_at",
		"suspension_origin",
		"hide_collections",
		"enable_rss",
	}
}

// stubbifyUser renders the given user as a stub,
// removing sensitive information like IP addresses
// and sign-in times, but keeping email addresses to
// prevent the same email address from creating another
// account on this instance.
//
// `encrypted_password` is set to the bcrypt hash of a
// random uuid, so if the action is reversed, the user
// will have to reset their password via email.
//
// For caller's convenience, this function returns the db
// names of all columns that are updated by it.
func stubbifyUser(user *gtsmodel.User) ([]string, error) {
	uuid, err := uuid.New().MarshalBinary()
	if err != nil {
		return nil, err
	}

	dummyPassword, err := bcrypt.GenerateFromPassword(uuid, bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	var never = time.Time{}

	user.EncryptedPassword = string(dummyPassword)
	user.SignUpIP = net.IPv4zero
	user.CurrentSignInAt = never
	user.CurrentSignInIP = net.IPv4zero
	user.LastSignInAt = never
	user.LastSignInIP = net.IPv4zero
	user.SignInCount = 1
	user.Locale = ""
	user.CreatedByApplicationID = ""
	user.LastEmailedAt = never
	user.ConfirmationToken = ""
	user.ConfirmationSentAt = never
	user.ResetPasswordToken = ""
	user.ResetPasswordSentAt = never

	return []string{
		"encrypted_password",
		"sign_up_ip",
		"current_sign_in_at",
		"current_sign_in_ip",
		"last_sign_in_at",
		"last_sign_in_ip",
		"sign_in_count",
		"locale",
		"created_by_application_id",
		"last_emailed_at",
		"confirmation_token",
		"confirmation_sent_at",
		"reset_password_token",
		"reset_password_sent_at",
	}, nil
}
