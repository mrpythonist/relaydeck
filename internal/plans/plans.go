// Package plans holds the hosted RelayDeck pricing tiers in one place.
// Change a cap here and it applies everywhere: ingestion limits,
// destination caps, retention pruning and the usage endpoint.
// Prices are in US dollars per month. No plan promises exactly-once
// delivery or zero loss; all plans are at-least-once, like the core.
package plans

// Plan is one hosted pricing tier.
type Plan struct {
	Name            string `json:"name"`
	PriceDollars    int    `json:"price_dollars"`
	MonthlyEvents   int64  `json:"monthly_events"`
	MaxDestinations int    `json:"max_destinations"`
	RetentionDays   int    `json:"retention_days"`
}

// Free is the $0 tier: 1,000 events a month, 3 destinations, 3 days retention.
var Free = Plan{Name: "free", PriceDollars: 0, MonthlyEvents: 1000, MaxDestinations: 3, RetentionDays: 3}

// Starter is the $19 tier: 50,000 events a month, 30 days retention.
var Starter = Plan{Name: "starter", PriceDollars: 19, MonthlyEvents: 50000, MaxDestinations: 0, RetentionDays: 30}

// Growth is the $49 tier: 500,000 events a month, 90 days retention.
var Growth = Plan{Name: "growth", PriceDollars: 49, MonthlyEvents: 500000, MaxDestinations: 0, RetentionDays: 90}

// All lists the plans in price order.
var All = []Plan{Free, Starter, Growth}

// Get returns the plan for name, or Free for an unknown or empty name.
func Get(name string) Plan {
	switch name {
	case "starter":
		return Starter
	case "growth":
		return Growth
	default:
		return Free
	}
}

// IsPaid reports whether name is a paid plan.
func IsPaid(name string) bool {
	return name == "starter" || name == "growth"
}

// Valid reports whether name is a known plan name.
func Valid(name string) bool {
	return name == "free" || name == "starter" || name == "growth"
}
