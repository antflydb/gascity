package main

import "time"

// prepareBudgetOpts is the one place the prepare-budget tests name the option,
// so the behaviour tests compile before the option exists and fail on their
// deferral assertions (ga-r7s62t RED). The implementation commit points it at
// withPrepareBudget.
func prepareBudgetOpts(time.Duration) []startExecutionOption { return nil }
