package hub

// MaxDeliveryKeyLen is maxDeliveryKeyLen, for the test that holds it to the column's own CHECK.
const MaxDeliveryKeyLen = maxDeliveryKeyLen

// MaxStackInLog is maxStackInLog, for the test that makes a provider recurse past it. Asserting
// against the constant rather than against a number written in the test keeps the two together if
// the bound is ever retuned.
const MaxStackInLog = maxStackInLog

// RefusedTypeName is refusedTypeName, for the test that panics with a type built at run time.
const RefusedTypeName = refusedTypeName

// Retryable is retryable, for the test of the one error shape no integration test can produce on
// demand: a statement the server cancelled, which comes back with no context error in its chain.
var Retryable = retryable
