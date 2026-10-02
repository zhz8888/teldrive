package authn

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrCodeInvalid reports a login code that Telegram rejected as wrong or
	// expired. It is returned by TelegramLogin.VerifyCode implementations and maps
	// to HTTP 422.
	ErrCodeInvalid = errors.New("Telegram login code is invalid")

	// ErrPasswordRequired reports that Telegram accepted the login code but the
	// account has a two-step password. Implementations must express that as a
	// LoginStep with PasswordRequired set and a nil error, not by returning this
	// value: it exists as the gateway-internal signal Service.VerifyCode converts
	// into a pending flow.
	ErrPasswordRequired = errors.New("Telegram two-step password is required")

	// ErrPasswordInvalid reports a two-step password Telegram rejected. It maps to
	// HTTP 401 and, like ErrCodeInvalid, says nothing about whether the account
	// exists or which part of the password was wrong.
	ErrPasswordInvalid = errors.New("Telegram two-step password is invalid")

	// ErrLoginStateInvalid reports gateway state this service can no longer resume:
	// a missing or half-populated LoginStep, or sealed flow state that names no
	// usable phone number or login session. Callers must treat it as "restart the
	// login" instead of retrying the same flow.
	ErrLoginStateInvalid = errors.New("Telegram login state is invalid")
)

// TelegramUser is the account a successful Telegram login reports. It is the
// gateway's view of the Telegram account, not the stored TelDrive user row.
type TelegramUser struct {
	// ID is the Telegram user ID, which TelDrive also uses as the account key; it
	// must be positive.
	ID int64
	// DisplayName is the account's first and last name joined by a space, and may
	// be empty for accounts that set no name.
	DisplayName string
	// Username is the public handle without a leading "@", and may be empty.
	Username string
	// Premium reports Telegram Premium membership as seen at login time and is
	// informational only.
	Premium bool
}

// LoginStep is one observation of a Telegram login, returned by every
// TelegramLogin call.
//
// A step is either still in progress, in which case State carries the opaque
// session the next call must resume from together with a QR link or a password
// prompt, or finished, in which case User and Session describe the authorized
// account. State is gateway-private: TelDrive stores it sealed and never
// interprets it.
type LoginStep struct {
	// State is the opaque resumable gateway state produced by this step and passed
	// back to the next TelegramLogin call. It is empty on a completed login.
	State []byte
	// User is set once Telegram authorized an account; nil means the login is still
	// pending.
	User *TelegramUser
	// Session is the authorized Telegram session string, set together with User and
	// sealed before storage. It is a credential and must never be logged.
	Session []byte
	// PasswordRequired reports that Telegram wants the account's two-step password
	// before this login can finish.
	PasswordRequired bool
	// QRURL is the tg:// link to render while a QR login is pending. It is empty
	// for phone logins and once the password prompt is reached.
	QRURL string
	// QRExpiresAt is when QRURL stops working, as reported by Telegram.
	QRExpiresAt time.Time
}

// TelegramLogin is the gateway contract the authentication service relies on to
// run Telegram logins.
//
// Implementations own all contact with Telegram and must be safe for concurrent
// calls, because the service processes different flows in parallel; state passed
// in and returned stays opaque to the caller. Every method reports an error
// instead of a partially filled step, and the sentinel errors above are the only
// failures callers distinguish. The stub used when Telegram is not configured
// fails every call.
type TelegramLogin interface {
	// Start sends a login code to phone and returns the state VerifyCode needs to
	// submit it. A blank phone number is rejected with ErrLoginStateInvalid.
	Start(context.Context, string) (LoginStep, error)
	// StartQR begins a QR login and returns a step carrying QRURL and QRExpiresAt,
	// or PasswordRequired when the account's two-step password is needed first.
	StartQR(context.Context) (LoginStep, error)
	// PollQR continues a QR login from previously returned state. It returns a step
	// with User and Session once the phone approved the code, and otherwise a
	// refreshed QR link; state that does not belong to a QR login is rejected with
	// ErrLoginStateInvalid.
	PollQR(context.Context, []byte) (LoginStep, error)
	// VerifyCode submits the code Telegram sent to phone, resuming from the state
	// Start returned. A wrong or expired code reports ErrCodeInvalid, while an
	// account with a two-step password yields a pending step with PasswordRequired
	// set rather than an error.
	VerifyCode(context.Context, string, []byte, string) (LoginStep, error)
	// VerifyPassword submits the account's two-step password, resuming from the
	// state of a password-required step. A rejected password reports
	// ErrPasswordInvalid, and the returned step carries the authorized user and
	// session.
	VerifyPassword(context.Context, []byte, string) (LoginStep, error)
}
