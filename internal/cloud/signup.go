package cloud

import (
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
)

// Signup creates an account (unverified, no 2FA) and emails a verification link. It answers the
// same whether or not the address was already registered.
//
// In "invite" mode every submission needs a live invitation, whatever it would have done (a new
// account, a repeat for an unverified address, a notice to an existing one), and spends it in the
// same transaction as that write. So a token that survives a submission says nothing about the
// address, and a stranger without one cannot touch a pending sign-up. A wrong token, and a token
// bound to another address, are ErrInviteInvalid and spend nothing. "closed" refuses everything.
func (a *Accounts) Signup(ip, rawEmail, password, invite string) error {
	mode := a.signupMode()
	if mode == SignupClosed {
		return ErrSignupClosed
	}
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return &PasswordError{"Enter a valid email address."}
	}
	bound := false
	if mode == SignupInvite {
		var err error
		if _, bound, err = a.inviteRow(a.db, invite, email, true); err != nil {
			return a.inviteFailure(ip, err)
		}
	}
	if err := auth.ValidateNewPassword(password); err != nil {
		return &PasswordError{err.Error()}
	}
	if !a.rate.Allow("signup:ip:"+ip, 10, time.Hour) || !a.rate.Allow("signup:email:"+email, 5, time.Hour) {
		return &LockedError{RetryAfter: time.Hour}
	}
	// Whatever the sign-up turns out to send (a verification link, or a notice to an existing
	// account), it is limited like a resend, and the same for new and existing addresses.
	if err := a.mailGate(mailVerify, ip, email); err != nil {
		return err
	}
	release, err := a.guard(ip)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password) // always: same work for new and existing addresses
	release()
	if err != nil {
		return err
	}

	// One transaction (a single writer: the store opens them with BEGIN IMMEDIATE) holds the
	// branch's write and the spending of the invitation. Events and mail wait until it commits.
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var after []func()
	var uid string
	owns := false // this sign-up created the account, or replaced the pending one: only then is it recorded as the invitation's account
	u, err := a.userByQ(tx, "email = ?", email)
	if err != nil {
		return err
	}
	switch {
	case u == nil:
		uid = newID()
		owns = true
		if _, err := tx.Exec("INSERT INTO users (id, email, password_hash, created_at) VALUES (?, ?, ?, ?)", uid, email, hash, a.now().UnixMilli()); err != nil {
			return err
		}
		tok, err := a.newEmailTokenQ(tx, uid, "verify")
		if err != nil {
			return err
		}
		after = append(after, func() { a.event(uid, "signup", ip, nil); a.sendMail(mailVerify, a.verifyMessage(email, tok)) })
	case u.VerifiedAt == 0 || u.EnrolledAt == 0:
		uid = u.ID
		// Nobody has proved they own a pending address yet, so the newest sign-up wins: someone who
		// typed it before its owner must not keep it. The password becomes the new one and the older
		// verification links stop working; whoever can read the mailbox and knows the newest password
		// finishes the sign-up. In invite mode only an invitation bound to this address may do that:
		// with an unbound one (any invitee holds one) the pending password stays, so one invitee
		// cannot lock another out. The answer is the same and the invitation is spent either way.
		if u.VerifiedAt == 0 && (mode != SignupInvite || bound) {
			owns = true
			if _, err := tx.Exec("UPDATE users SET password_hash = ? WHERE id = ? AND email_verified_at IS NULL", hash, u.ID); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE email_tokens SET used_at = ? WHERE user_id = ? AND kind = 'verify' AND used_at IS NULL", a.now().UnixMilli(), u.ID); err != nil {
				return err
			}
		}
		// Verified but 2FA unfinished: the same link works (it asks for the password).
		tok, err := a.newEmailTokenQ(tx, u.ID, "verify")
		if err != nil {
			return err
		}
		after = append(after, func() { a.event(uid, "signup_repeat", ip, nil); a.sendMail(mailVerify, a.verifyMessage(email, tok)) })
	default:
		uid = u.ID
		after = append(after, func() {
			a.event(uid, "signup_existing", ip, nil)
			a.sendMail(mailNotice, Message{To: email, Subject: "Maestria: you already have an account", Body: "Someone tried to create a Maestria account with this address, but one already exists.\n\nIf it was you, log in at " + a.url + "/login (use \"Forgot password?\" if needed).\nIf it was not, you can ignore this email."})
		})
	}
	if mode == SignupInvite {
		id, err := a.useInvite(tx, invite, email, ownerOf(owns, uid))
		if err != nil {
			return a.inviteFailure(ip, err)
		}
		after = append(after, func() { a.event(uid, "invite_used", ip, map[string]any{"invite": id}) })
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, f := range after {
		f()
	}
	return nil
}

// inviteFailure turns a failed check into the one answer a visitor gets, and counts it against
// their client address.
func (a *Accounts) inviteFailure(ip string, err error) error {
	if err == ErrInviteInvalid {
		return a.failedInvite(ip)
	}
	return err
}

func (a *Accounts) verifyMessage(email, tok string) Message {
	return Message{To: email, Subject: "Verify your email for Maestria",
		Body: "Confirm your email to finish creating your Maestria account:\n\n" + a.url + "/verify/" + tok + "\n\nThe link expires in 30 minutes. If you did not sign up, ignore this email."}
}

// ownerOf is the account an invitation is recorded against: the one this sign-up created or took
// over, and nothing for a repeat that left an existing or pending account alone (so the list never
// says someone used an invitation they did not touch).
func ownerOf(owns bool, uid string) string {
	if owns {
		return uid
	}
	return ""
}
