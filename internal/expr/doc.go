// Package expr compiles the expressions metric rules are written in — jq,
// regular expressions, CSS selectors and XPath — and keeps each compiled
// program in a bounded cache, so a rule is compiled once rather than on every
// scrape.
package expr
