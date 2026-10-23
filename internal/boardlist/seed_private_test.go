//go:build private

package boardlist

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestParseSeedLists runs ParseCSV over the private seed lists in
// LEADHOUND_DATA_DIR/boards/{greenhouse,lever,ashby}.csv and logs counts and refusal
// reasons only, never a value from the files.
func TestParseSeedLists(t *testing.T) {
	dir := os.Getenv("LEADHOUND_DATA_DIR")
	if dir == "" {
		t.Fatal("LEADHOUND_DATA_DIR is not set")
	}
	for _, name := range []string{"greenhouse", "lever", "ashby"} {
		f, err := os.Open(filepath.Join(dir, "boards", name+".csv")) // #nosec G304 G703 -- the operator's own data dir
		if err != nil {
			t.Fatal(err)
		}
		entries, refused, err := ParseCSV(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		unique := len(Dedupe(entries))
		byReason := map[string]int{}
		for _, r := range refused {
			byReason[r.Reason]++
		}
		reasons := make([]string, 0, len(byReason))
		for r := range byReason {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool { return byReason[reasons[i]] > byReason[reasons[j]] })
		t.Logf("%s: parsed %d (unique %d), refused %d", name, len(entries), unique, len(refused))
		for _, r := range reasons {
			t.Logf("  %5d  %s", byReason[r], r)
		}
		for _, e := range entries {
			if e.Board.ATS != name {
				t.Errorf("%s: an entry maps to %s", name, e.Board.ATS)
				break
			}
		}
	}
}
