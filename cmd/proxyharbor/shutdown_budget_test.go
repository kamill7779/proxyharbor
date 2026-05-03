package main

import (
	"testing"
	"time"
)

func TestShutdownBudgetPlanPreservesTotal(t *testing.T) {
	drain := shutdownDrainDelay(15 * time.Second)
	budgets := splitShutdownBudget(15*time.Second-drain, 4)
	if len(budgets) != 4 {
		t.Fatalf("len(budgets) = %d, want 4", len(budgets))
	}
	total := drain
	for i, budget := range budgets {
		if budget < 0 {
			t.Fatalf("budget[%d] = %v, want non-negative", i, budget)
		}
		total += budget
	}
	if total != 15*time.Second {
		t.Fatalf("total budget = %v, want 15s", total)
	}
}
