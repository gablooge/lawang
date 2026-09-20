package outbox

import "github.com/gablooge/lawang/internal/tenancy"

// AcceptIn is Accept inside a caller's tenant-bound transaction, so that a test can hold an
// accepting transaction open.
var AcceptIn = acceptIn

// MarkDeliveredIn is MarkDelivered inside a caller's tenant-bound transaction, so that a test can
// hold a finishing transaction open.
var MarkDeliveredIn = markDeliveredIn

// WithTenant returns a genuine Claimed aimed at another tenant, token and all, which nothing
// outside this package can build. It is for the test that shows such a value changes nothing.
func WithTenant(c Claimed, tenant tenancy.ID) Claimed {
	c.tenant = tenant
	return c
}

// Clip is clip, the last line of defense in front of the two text columns.
var Clip = clip
