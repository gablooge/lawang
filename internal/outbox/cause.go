package outbox

import (
	"fmt"
	"strings"
)

// Class says what kind of failure stopped an attempt. The classes follow the failure table in
// docs/architecture.md section 11. A later item that meets a failure of another kind adds a class
// here, with its text: it does not get a way to pass text in.
type Class uint8

// The classes of failure.
const (
	// ClassUnclassified is the zero value: a Cause that was never given a class.
	ClassUnclassified Class = iota
	// ClassNormalizer: the stored body could not be parsed or normalized. Not retryable.
	ClassNormalizer
	// ClassProviderUnavailable: the provider's API timed out, refused the connection, rate limited
	// the call or answered 5xx.
	ClassProviderUnavailable
	// ClassSinkUnavailable: the sink timed out, refused the connection or answered 5xx.
	ClassSinkUnavailable
	// ClassSinkRejected: the sink refused the record itself. Not retryable.
	ClassSinkRejected
	// ClassSinkUnauthorized: the sink refused the credential (401) or lacks a grant (403).
	ClassSinkUnauthorized
	// ClassVaultUnavailable: a secret could not be read, so nothing may be delivered.
	ClassVaultUnavailable
	// ClassInternal: a failure of Lawang's own, such as its database.
	ClassInternal
)

var classText = map[Class]string{
	ClassUnclassified:        "unclassified failure",
	ClassNormalizer:          "normalizer failed",
	ClassProviderUnavailable: "provider unavailable",
	ClassSinkUnavailable:     "sink unavailable",
	ClassSinkRejected:        "sink rejected the record",
	ClassSinkUnauthorized:    "sink refused the credential",
	ClassVaultUnavailable:    "vault unavailable",
	ClassInternal:            "internal error",
}

// maxCodeLen bounds a provider's or sink's error code.
const maxCodeLen = 64

// codeWithheld stands in for a code that did not look like one.
const codeWithheld = "withheld"

// Cause is why an attempt failed, in the only form the outbox stores. It ends up in last_error, a
// plain text column that operators read and every backup carries, so it must never hold token
// material, and the way to be sure of that is to never let text from outside in.
//
// The accident this prevents is the obvious call: recording err.Error(). The error of an HTTP
// client is a *url.Error, and its text quotes the request URL with its query string, which for
// many webhook sinks and provider APIs is where the API key travels. So a Cause is built from a
// Class, which is one of this package's own texts, and at most two facts about the response: its
// HTTP status, a number, and the remote system's error code, which is kept only if it looks like
// one. There is deliberately no constructor that takes an error or a message.
//
// The zero Cause is valid and reads "unclassified failure".
type Cause struct {
	class  Class
	status int
	code   string
}

// NewCause returns a Cause of the given class.
func NewCause(class Class) Cause { return Cause{class: class} }

// WithStatus adds the HTTP status of the response that failed. A number that is not a status
// (outside 100 to 599) is left out.
func (c Cause) WithStatus(status int) Cause {
	if status >= 100 && status <= 599 {
		c.status = status
	}
	return c
}

// WithCode adds the error code the remote system gave, such as "rate_limited" or "invalid_grant".
// It is kept only if it is at most 64 bytes of ASCII letters, digits, '_', '-' and '.', which a
// message, a URL, a header or a JSON fragment never is. Anything else is recorded as "withheld",
// so that an operator can see there was a code and that Lawang chose not to store it.
//
// This stops text that carries a secret by accident. It cannot stop a caller that passes a secret
// as the code: a bare token looks like a code. Pass the field of the response that the remote
// system documents as its error code, and nothing else.
func (c Cause) WithCode(code string) Cause {
	if code == "" {
		return c
	}
	c.code = codeWithheld
	if len(code) <= maxCodeLen && strings.IndexFunc(code, notCodeChar) < 0 {
		c.code = code
	}
	return c
}

func notCodeChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		return false
	}
	return true
}

// String is the text that is stored, for example "sink unavailable (status 503, code overloaded)".
func (c Cause) String() string {
	text, ok := classText[c.class]
	if !ok {
		text = classText[ClassUnclassified]
	}
	switch {
	case c.status != 0 && c.code != "":
		return fmt.Sprintf("%s (status %d, code %s)", text, c.status, c.code)
	case c.status != 0:
		return fmt.Sprintf("%s (status %d)", text, c.status)
	case c.code != "":
		return fmt.Sprintf("%s (code %s)", text, c.code)
	}
	return text
}
