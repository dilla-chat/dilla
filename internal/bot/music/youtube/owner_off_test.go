package youtube

// Most tests exercise the resolver and starter without naming an owner. Shipped
// builds refuse that (requireOwner); the owner tests turn the refusal back on.
var shippedRequireOwner = requireOwner

func init() { requireOwner = false }
