package leadstore

import (
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// A lead's rating type is the offer its relevance is judged against, derived from the
// lead's kind: every café kind is a café lead, every other kind (the Drupal site kinds,
// agencies, hand-added sites, selftest and any unknown kind) a Drupal lead. Each type has
// its own relevance question, its own calls and its own trust gate.
const (
	TypeDrupal = "drupal"
	TypeCafe   = "cafe"
)

// RatingTypes lists the rating types in display order.
var RatingTypes = []string{TypeDrupal, TypeCafe}

// The relevance question of each type, as lead_relevance.question and
// lead_relevance_eval.question store it. QuestionDrupal is the question every rating made
// before café leads had their own was stored under.
const (
	QuestionDrupal = "relevance@1"
	QuestionCafe   = "relevance-cafe@1"
)

// TypeQuestion is rating type t's relevance question; anything that is not a known type
// is a Drupal lead, as TypeOf says.
func TypeQuestion(t string) string {
	if t == TypeCafe {
		return QuestionCafe
	}
	return QuestionDrupal
}

// TypeOf is the rating type of a lead of this kind.
func TypeOf(kind string) string {
	if slices.ContainsFunc(cafe.Kinds, func(k cafe.KindInfo) bool { return k.Kind == kind }) {
		return TypeCafe
	}
	return TypeDrupal
}

// TypeWords is rating type t in words, for the console.
func TypeWords(t string) string {
	if t == TypeCafe {
		return "Café leads"
	}
	return "Drupal leads"
}

// cafeKindsSQL is the café kinds as a SQL list of string literals, generated from
// cafe.Kinds so the SQL and TypeOf can never disagree.
func cafeKindsSQL() string {
	lits := make([]string, len(cafe.Kinds))
	for i, k := range cafe.Kinds {
		lits[i] = sqlLiteral(k.Kind)
	}
	return strings.Join(lits, ", ")
}

// typeSQL is the SQL expression giving the rating type of the lead whose kind is in
// kindCol, as TypeOf does.
func typeSQL(kindCol string) string {
	return "CASE WHEN " + kindCol + " IN (" + cafeKindsSQL() + ") THEN " + sqlLiteral(TypeCafe) +
		" ELSE " + sqlLiteral(TypeDrupal) + " END"
}

// typeQuestionSQL is the SQL expression giving the relevance question of the lead whose
// kind is in kindCol, as TypeQuestion(TypeOf(kind)) does.
func typeQuestionSQL(kindCol string) string {
	return "CASE WHEN " + kindCol + " IN (" + cafeKindsSQL() + ") THEN " + sqlLiteral(QuestionCafe) +
		" ELSE " + sqlLiteral(QuestionDrupal) + " END"
}

// sqlLiteral quotes s as a SQL string literal, doubling any single quote.
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
