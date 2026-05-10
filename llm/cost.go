package llm

// CostSummary holds the total cost breakdown for a session.
type CostSummary struct {
	MainCost     float64 // Cost from main session messages
	SubagentCost float64 // Cost from all subagent messages
	TotalCost    float64 // MainCost + SubagentCost
}

// CalculateUsageCosts computes total cost from a slice of Usage entries using the given price.
// Each usage entry represents one API call's token consumption.
func CalculateUsageCosts(usages []Usage, price *ModelPrice) float64 {
	if price == nil {
		return 0
	}
	var total float64
	for i := range usages {
		total += CalculateCost(&usages[i], price)
	}
	return total
}
