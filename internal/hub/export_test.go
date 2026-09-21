package hub

// MaxDeliveryKeyLen is maxDeliveryKeyLen, for the test that holds it to the column's own CHECK.
const MaxDeliveryKeyLen = maxDeliveryKeyLen

// Retryable is retryable, for the test of the one error shape no integration test can produce on
// demand: a statement the server cancelled, which comes back with no context error in its chain.
var Retryable = retryable
