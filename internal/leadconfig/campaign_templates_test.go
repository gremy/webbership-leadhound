package leadconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// campaignTemplatesDir writes step 0 for kinds in "en" and the given extra files.
func campaignTemplatesDir(t *testing.T, kinds []string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	for _, k := range kinds {
		write(k+"/en/0.txt", "Subject: Hello {{.OrgName}}\n\nHello.\n\n{{.Footer}}\n")
	}
	for rel, text := range files {
		write(rel, text)
	}
	return dir
}

// A campaign's form.txt and linkedin.txt: optional (both or neither) for a kind leads.yaml
// has templates for, required for any other kind; loaded into the Set when present.
func TestLoadCampaignFormTemplates(t *testing.T) {
	const cafe = "cafe_no_website"
	form := map[string]string{cafe + "/en/" + FormFile: "Hello {{.OrgName}}, {{.SenderName}}.\n", cafe + "/en/" + LinkedInFile: "Hello {{.OrgName}}.\n"}
	cases := []struct {
		name  string
		kind  string
		files map[string]string
		want  string // "" loads; else the error holds it
		form  bool
	}{
		{"d10 without form texts", KindD10, nil, "", false},
		{"d10 with both", KindD10, map[string]string{"d10/en/form.txt": "Hi.\n", "d10/en/linkedin.txt": "Hi.\n"}, "", true},
		{"d10 with form only", KindD10, map[string]string{"d10/en/form.txt": "Hi.\n"}, "d10/en/linkedin.txt is missing", false},
		{"café with both", cafe, form, "", true},
		{"café without", cafe, nil, cafe + "/en/form.txt is missing", false},
		{"café form unparsable", cafe, map[string]string{cafe + "/en/form.txt": "{{.Nope}}\n", cafe + "/en/linkedin.txt": "Hi.\n"}, "form.txt does not render", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := campaignTemplatesDir(t, []string{c.kind}, c.files)
			tm, err := LoadCampaignTemplates(dir, CampaignTemplates{Kinds: []string{c.kind}, Langs: []string{"en"}, EmailSteps: []int{0}})
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err %v, want %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			set, _ := tm.Set(c.kind, "en")
			if got := set.Form != nil && set.LinkedIn != nil; got != c.form {
				t.Errorf("form texts loaded = %v, want %v", got, c.form)
			}
		})
	}
}
