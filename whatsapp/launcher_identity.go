package whatsapp

import "context"

// UserStore resolves a WhatsApp sender to a user of whatever system the product
// keeps accounts in, creating one the first time a number is seen.
//
// It is the small answer to the question [Gate] answers in full: a WhatsApp
// sender is a phone number, and a phone number is not an account. A store closes
// that gap with the least the launcher can work with — find, and failing that,
// create — and holds the one opinion this package does have about identity: a
// user exists before a session does.
//
// Nothing here says what a user is. Whatever the product's definition, it needs
// an id and it must be addressable by WhatsApp number; the launcher neither sees
// nor stores anything else. Supply a store as [Config].Users.
//
// A store cannot refuse — finding nobody means creating somebody. Refusing is
// [Gate]'s job, and a gate runs between the two halves of this interface so that
// a sender it turns away is never created. What neither can express is a refusal
// that becomes an account later on different terms; a sender who is admitted is
// admitted as the holder of their number.
//
// Note what that means: possession of the number is the whole credential and
// nothing expires, so a reassigned number inherits the previous holder's account
// for as long as it stays bound.
type UserStore interface {
	// FindByPhoneNumber returns the id of the user holding phoneE164, or "" when
	// no user holds it. A "" id with a nil error is the not-found signal, so a
	// store must not report an unknown number as an error — that fails the turn.
	FindByPhoneNumber(ctx context.Context, phoneE164 string) (userID string, err error)

	// CreateByPhoneNumber creates a user holding phoneE164 and returns its id.
	//
	// Called only when FindByPhoneNumber found none. The number arrived on a
	// signature-verified webhook, so possession of it is proven; nothing else
	// about the sender is known, and the launcher has nothing else to offer.
	CreateByPhoneNumber(ctx context.Context, phoneE164 string) (userID string, err error)
}
