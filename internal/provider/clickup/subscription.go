package clickup

import (
	"errors"
	"fmt"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/tenancy"
)

// ErrBadRegistration reports a registration this provider will not store. It names the field and
// never its value: one of the fields is a secret.
var ErrBadRegistration = errors.New("clickup: bad registration")

// SubscriptionFor is the one shape a ClickUp registration may take, and the registrar of B14
// builds its rows with it rather than filling a provider.Subscription by hand.
//
// # Why this is a function and not a struct literal in the registrar
//
// Three things about the row decide whether a workspace's deliveries are ever routed, and two of
// them are easy to get wrong in a way nothing reports until deliveries start arriving.
//
//  1. **External is the ClickUp webhook id, and it is required.** A ClickUp webhook body carries
//     no workspace or team id at all (https://developer.clickup.com/docs/webhooktaskpayloads);
//     the only identifier on it is webhook_id, which is what DeliveryKeys reads. A row that
//     recorded the workspace and not the webhook id would be selected by no delivery ever, and
//     every delivery of that workspace would be parked as "no owner", permanently and silently.
//
//  2. **Workspace is empty**, because provider.Subscription.Workspace is the id "as it will
//     appear on a delivery", and ClickUp puts none there. Recording one would be recording a key
//     nothing is ever looked up by.
//
//  3. **Resource is the ClickUp workspace (team) id.** That is what the registration covers, and
//     it is also what hub.Subscriptions.Register upserts on: a second Register for the same
//     (tenant, provider, resource) updates the row in place instead of adding another. So a
//     tenant cannot end up with two ClickUp rows for one workspace, which is exactly what
//     [ADR 11](../../../docs/adr/0011-hub-resolution.md) decision 5 requires of a registration
//     design, and the reason no unique index was added for it
//     ([ADR 15](../../../docs/adr/0015-clickup-provider.md)).
//
// The consequence for the registrar: Lawang creates ONE ClickUp webhook per workspace per
// tenant, at workspace level with no list, folder or space filter, and any narrowing of what is
// ingested is done inside that one row's reach rather than by a second webhook. Re-registering
// replaces the row, so the registrar deregisters the webhook it replaced, or ClickUp goes on
// posting deliveries for a webhook id no row records.
func SubscriptionFor(t tenancy.ID, workspaceID, webhookID string, secret []byte) (provider.Subscription, error) {
	if _, err := tenancy.Parse(t.String()); err != nil {
		return provider.Subscription{}, fmt.Errorf("%w: tenant: %w", ErrBadRegistration, err)
	}
	if !validID(workspaceID) {
		return provider.Subscription{}, fmt.Errorf("%w: the workspace id is missing or is not an identifier", ErrBadRegistration)
	}
	if !validID(webhookID) {
		return provider.Subscription{}, fmt.Errorf("%w: the webhook id is missing or is not an identifier", ErrBadRegistration)
	}
	if len(secret) == 0 {
		// An empty secret verifies nothing, so the row would answer 401 to every delivery it was
		// made for, and only ClickUp's own dashboard would show it.
		return provider.Subscription{}, fmt.Errorf("%w: the webhook secret is empty", ErrBadRegistration)
	}
	return provider.Subscription{
		Tenant:    t,
		Provider:  Key,
		Resource:  workspaceID,
		Workspace: "", // ClickUp puts no workspace id on a delivery
		External:  webhookID,
		Secret:    secret,
	}, nil
}
