package diagnosis

// Code is a root-cause code the agent may report.
type Code struct {
	Code        string
	Description string
}

// Codes is the closed set of root-cause codes for the demo system. It is
// shown to the agent, so it must stay scenario-neutral: every scenario's
// answer is one of several plausible codes, each distinguishable only with
// evidence. See docs/adr/0002-closed-root-cause-taxonomy.md.
var Codes = []Code{
	{"INVENTORY_DOWNSTREAM_LATENCY", "inventory-api itself responds slowly, slowing its callers"},
	{"CHECKOUT_INTERNAL_LATENCY", "checkout-api is slow while its dependencies respond normally"},
	{"CHECKOUT_RESOURCE_SATURATION", "checkout-api is short of CPU or memory"},
	{"CHECKOUT_INVENTORY_NETWORK_LATENCY", "calls from checkout-api to inventory-api are slow in transit while inventory-api serves them quickly"},
	{"INVENTORY_ERRORS", "inventory-api returns errors"},
	{"INVENTORY_UNAVAILABLE", "inventory-api is down or unreachable"},
	{"NO_ISSUE_FOUND", "the evidence shows no anomaly"},
	{"UNKNOWN", "the evidence is inconclusive"},
}

// IsKnownCode reports whether code is in Codes.
func IsKnownCode(code string) bool {
	for _, c := range Codes {
		if c.Code == code {
			return true
		}
	}
	return false
}
