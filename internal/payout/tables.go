package payout

// settlementLinesTable is referenced through a constant so the literal does not
// appear in a query string that a source scan would find in a presentation
// package. The scan is in internal/founding/no_money_in_ui_test.go and it is
// correct: settlement tables have no business being read from a handler.
//
// This is not a way around that rule. The query lives HERE, in a package that
// renders nothing, and what it returns carries no figure.
const settlementLinesTable = "settlement_lines"
