// Package moderation keeps the Bot's moderation evidence. Bot v2 Task 23 (fix round 1) removed the AutoModeration engine
// (its evaluator, checkpoint and configuration store); the rule and action names stay because stored evidence and the
// moderation log's automatic cases carry them.
package moderation

import "time"

const (
	// MaxTimeout bounds a stored timeout action.
	MaxTimeout = 28 * 24 * time.Hour
	// ExcerptMaxBytes bounds a stored message excerpt.
	ExcerptMaxBytes = 512
)

// Rule names why a case was recorded.
type Rule string

const (
	RuleRate      Rule = "message_rate"
	RuleDuplicate Rule = "duplicate_body"
	RuleMentions  Rule = "mention_count"
	RuleManual    Rule = "manual_moderator"
)

// Action names what a case did.
type Action string

const (
	ActionLog     Action = "log"
	ActionRemove  Action = "remove"
	ActionWarn    Action = "warn"
	ActionTimeout Action = "timeout"
)

func validRule(rule Rule) bool {
	return rule == RuleRate || rule == RuleDuplicate || rule == RuleMentions
}
