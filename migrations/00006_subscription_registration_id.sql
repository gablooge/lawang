-- +goose Up

-- One registration id is one row, within one tenant and one provider.
--
-- `external_id` is the provider's own id of the registration, and the accept path selects
-- candidate subscriptions by it (migration 00003, `subscriptions_by_external`). So two rows of
-- one tenant carrying one registration id are both candidates for every delivery that names it,
-- both verify (one registration has one secret), and internal/hub parks the delivery as an
-- ambiguous owner: permanently, and with nothing but a parked row to show for it. The table's
-- other uniqueness, (tenant_id, provider, resource), does not reach this: the two rows cover two
-- resources, which is a legal and ordinary thing for them to do.
--
-- This is the constraint ADR 11 decision 5 left open and ADR 15 decision 3 settles. It is the
-- registration id and NOT the workspace, which is what makes it safe for every provider:
-- Microsoft Graph registers one subscription per resource and several rows of one tenant on one
-- workspace are correct there, but each of those rows carries its own Graph subscription id, so
-- none of them collides here.
--
-- It is per tenant and not global. Two tenants naming one registration id is not a state Lawang
-- can rule out (a provider's id space is not this deployment's to make unique across accounts)
-- and not one it has to: the hub decides between two tenants' rows on their secrets, routing when
-- one verifies and parking when both do, which is designed cross-tenant behaviour with tests of
-- its own in internal/hub.
--
-- The predicate is needed because a provider that sends no registration id leaves the column
-- empty (migration 00003's CHECK allows that as long as the workspace id is there), and every
-- such row of a tenant would otherwise collide with every other. A partial index is right here
-- where it would be wrong for the lookup beside it: that one is skipped under a generic plan
-- because the planner cannot prove a parameter is non-empty, and a constraint is checked against
-- the row's own value, which is never a parameter.
CREATE UNIQUE INDEX subscriptions_one_registration_per_tenant
  ON subscriptions (tenant_id, provider, external_id)
  WHERE external_id <> '';

-- +goose Down
DROP INDEX subscriptions_one_registration_per_tenant;
