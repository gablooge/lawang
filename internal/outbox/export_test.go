package outbox

// AcceptIn is Accept inside a caller's tenant-bound transaction, so that a test can hold an
// accepting transaction open.
var AcceptIn = acceptIn
