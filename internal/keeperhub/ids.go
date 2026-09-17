package keeperhub

import "github.com/google/uuid"

// uuidLike supplies JSON-RPC request ids.
//
// Trivially separate from newIdempotencyKey on the Client, and deliberately so:
// that one is a field a test can replace and whose freshness is load-bearing
// for money, while this one only correlates a request with its response. They
// must never be confused, so they are not the same function.
func uuidLike() string { return uuid.NewString() }
