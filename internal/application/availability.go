package application

import (
	"fmt"

	"aninode/internal/candidate"
	"aninode/internal/inventory"
)

// gateAvailability is the single filesystem availability gate for both
// current RSS and historical candidates. Unknown is deliberately suppressive.
func gateAvailability(plan *candidate.Plan, inv inventory.Snapshot) []string {
	unknown := map[string]bool{}
	var warnings []string
	for i := range plan.Candidates {
		value := &plan.Candidates[i]
		if value.Decision != candidate.DecisionSelected {
			continue
		}
		switch inv.Availability(value.Episode) {
		case inventory.Present:
			value.Decision = candidate.DecisionDuplicate
			value.Reason = "logical episode is present in library"
		case inventory.Unknown:
			value.Decision = candidate.DecisionFiltered
			value.Reason = "filesystem inventory is unknown; automatic acquisition suppressed for this episode"
			scope := fmt.Sprintf("%s/season-%02d", value.EntryKey, value.Episode.Season)
			if !unknown[scope] {
				unknown[scope] = true
				warnings = append(warnings, fmt.Sprintf("filesystem inventory unknown for %s; acquisition is suppressed only for that scope", scope))
			}
		}
	}
	return warnings
}
