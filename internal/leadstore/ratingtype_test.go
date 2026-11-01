package leadstore

import (
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// TestTypeOf: every kind a campaign may name, plus selftest, has the rating type its kind
// says: café kinds are café leads, everything else a Drupal lead.
func TestTypeOf(t *testing.T) {
	cafes := map[string]bool{}
	for _, k := range cafe.Kinds {
		cafes[k.Kind] = true
	}
	kinds := append(campaign.KnownKinds(), LeadSelftest, LeadHand, "no_such_kind", "")
	seenCafe, seenDrupal := 0, 0
	for _, k := range kinds {
		want := TypeDrupal
		if cafes[k] {
			want = TypeCafe
			seenCafe++
		} else {
			seenDrupal++
		}
		if got := TypeOf(k); got != want {
			t.Errorf("TypeOf(%q) = %q, want %q", k, got, want)
		}
		if got, want := TypeQuestion(TypeOf(k)), map[string]string{TypeDrupal: QuestionDrupal, TypeCafe: QuestionCafe}[want]; got != want {
			t.Errorf("TypeQuestion(TypeOf(%q)) = %q, want %q", k, got, want)
		}
	}
	if seenCafe != len(cafe.Kinds) {
		t.Errorf("KnownKinds holds %d café kinds, cafe.Kinds %d", seenCafe, len(cafe.Kinds))
	}
	for _, k := range []string{LeadUnsupported, LeadD10, LeadAgencyPartner, LeadHand, LeadSelftest} {
		if TypeOf(k) != TypeDrupal {
			t.Errorf("%s is not a Drupal lead", k)
		}
	}
	if seenDrupal == 0 {
		t.Error("no Drupal kind swept")
	}
}

// TestTypeSQL: the SQL expressions name every café kind and the two types' values, quoted
// as literals, so they agree with TypeOf and TypeQuestion.
func TestTypeSQL(t *testing.T) {
	ts, qs := typeSQL("l.kind"), typeQuestionSQL("l.kind")
	for _, k := range cafe.Kinds {
		for _, s := range []string{ts, qs} {
			if !strings.Contains(s, "'"+k.Kind+"'") {
				t.Errorf("%s lacks café kind %s", s, k.Kind)
			}
		}
	}
	if want := "CASE WHEN l.kind IN ("; !strings.HasPrefix(ts, want) || !strings.HasPrefix(qs, want) {
		t.Errorf("expressions do not read the kind column:\n%s\n%s", ts, qs)
	}
	if !strings.HasSuffix(ts, "THEN 'cafe' ELSE 'drupal' END") {
		t.Errorf("typeSQL = %s", ts)
	}
	if !strings.HasSuffix(qs, "THEN 'relevance-cafe@1' ELSE 'relevance@1' END") {
		t.Errorf("typeQuestionSQL = %s", qs)
	}
	if got := sqlLiteral("a'b"); got != "'a''b'" {
		t.Errorf("sqlLiteral = %s", got)
	}
}

func TestTypeWords(t *testing.T) {
	if TypeWords(TypeDrupal) != "Drupal leads" || TypeWords(TypeCafe) != "Café leads" {
		t.Errorf("words: %q, %q", TypeWords(TypeDrupal), TypeWords(TypeCafe))
	}
	if len(RatingTypes) != 2 || RatingTypes[0] != TypeDrupal || RatingTypes[1] != TypeCafe {
		t.Errorf("RatingTypes = %v", RatingTypes)
	}
}
