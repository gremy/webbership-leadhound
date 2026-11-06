//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestIGFollowDirections: one account on both of a vendor's lists in one capture is a row in
// each direction, a re-import stores nothing, an unknown direction is ErrBadIG, and
// IGFollowerVendors reads only the followers rows, each owner once with its latest capture.
func TestIGFollowDirections(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	both := []leadstore.IGFollowRow{{Handle: "both.example"}}
	for _, d := range []string{leadstore.IGFollowing, leadstore.IGFollowers} {
		if n, err := f.q.InsertIGFollows(ctx, "vendor.example", d, at, igSHA(1), both); err != nil || n != 1 {
			t.Fatalf("%s: %d %v, want 1 new", d, n, err)
		}
		if n, err := f.q.InsertIGFollows(ctx, "vendor.example", d, at, igSHA(1), both); err != nil || n != 0 {
			t.Fatalf("%s again: %d %v, want 0 new", d, n, err)
		}
	}
	if _, err := f.q.InsertIGFollows(ctx, "vendor.example", "fans", at, igSHA(2), both); !errors.Is(err, leadstore.ErrBadIG) {
		t.Errorf("unknown direction: err = %v, want ErrBadIG", err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_follow WHERE handle = 'both.example'`); n != 2 {
		t.Errorf("rows of both.example = %d, want 2 (one per direction)", n)
	}
	later := at.Add(48 * time.Hour)
	if _, err := f.q.InsertIGFollows(ctx, "vendor.example", leadstore.IGFollowers, later, igSHA(3), both); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertIGFollows(ctx, "other_vendor.example", leadstore.IGFollowers, at, igSHA(4), both); err != nil {
		t.Fatal(err)
	}
	// following.example is only on a following list: never a follower.
	if _, err := f.q.InsertIGFollows(ctx, "vendor.example", leadstore.IGFollowing, at, igSHA(5), []leadstore.IGFollowRow{{Handle: "following.example"}}); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.IGFollowerVendors(ctx, []string{"@Both.Example", "following.example", "not one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["following.example"]; ok || len(got) != 1 {
		t.Errorf("follower vendors = %+v, want only both.example", got)
	}
	b := got["both.example"]
	if len(b) != 2 || b[0].Owner != "other_vendor.example" || !b[0].CapturedAt.Equal(at) ||
		b[1].Owner != "vendor.example" || !b[1].CapturedAt.Equal(later) {
		t.Errorf("both.example follows %+v, want other_vendor.example at %v and vendor.example at its latest %v", b, at, later)
	}
}

// TestIGTouchesStoredOnce: touches are stored once by vendor, kind, handle and ref, whichever
// capture brings them again; a bad touch fails the whole call and stores nothing; IGTouchesFor
// reads a handle's touches newest first and leaves out those before since.
func TestIGTouchesStoredOnce(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	touches := []leadstore.IGTouch{
		{Handle: "@Cafe.Exemplu", Kind: leadstore.IGTouchComment, Ref: "17900000000000001", PostCode: "PostA", At: day(20),
			Text: "Cum functioneaza cardul?", FullName: "Cafe Exemplu"},
		{Handle: "cafe.exemplu", Kind: leadstore.IGTouchUsertag, Ref: "PostB", PostCode: "PostB", At: day(10)},
		{Handle: "cafe.exemplu", Kind: leadstore.IGTouchTagged, Ref: "OwnPost1", PostCode: "OwnPost1", At: day(1)},
		{Handle: "fan.example", Kind: leadstore.IGTouchMention, Ref: "PostB", PostCode: "PostB", At: day(10)},
	}
	if n, err := f.q.InsertIGTouches(ctx, "Vendor.Example", at, igSHA(1), touches); err != nil || n != 4 {
		t.Fatalf("first: %d %v, want 4 new", n, err)
	}
	if n, err := f.q.InsertIGTouches(ctx, "vendor.example", at.Add(time.Hour), igSHA(2), touches); err != nil || n != 0 {
		t.Fatalf("again from a new capture: %d %v, want 0 new", n, err)
	}
	if n, err := f.q.InsertIGTouches(ctx, "vendor.example", at, igSHA(1), nil); err != nil || n != 0 {
		t.Fatalf("none: %d %v", n, err)
	}
	ok := leadstore.IGTouch{Handle: "ok.example", Kind: leadstore.IGTouchComment, Ref: "c1", PostCode: "PostA", At: day(2)}
	for name, bad := range map[string]leadstore.IGTouch{
		"not a handle":      {Handle: "not one", Kind: leadstore.IGTouchComment, Ref: "c2", PostCode: "PostA", At: day(2)},
		"unknown kind":      {Handle: "a.example", Kind: "like", Ref: "c2", PostCode: "PostA", At: day(2)},
		"bad ref":           {Handle: "a.example", Kind: leadstore.IGTouchComment, Ref: "c 2", PostCode: "PostA", At: day(2)},
		"bad post code":     {Handle: "a.example", Kind: leadstore.IGTouchComment, Ref: "c2", PostCode: "", At: day(2)},
		"no date":           {Handle: "a.example", Kind: leadstore.IGTouchComment, Ref: "c2", PostCode: "PostA"},
		"text on a tag":     {Handle: "a.example", Kind: leadstore.IGTouchUsertag, Ref: "PostA", PostCode: "PostA", At: day(2), Text: "hi"},
		"text over the cap": {Handle: "a.example", Kind: leadstore.IGTouchComment, Ref: "c2", PostCode: "PostA", At: day(2), Text: strings.Repeat("ă", 301)},
	} {
		if _, err := f.q.InsertIGTouches(ctx, "vendor.example", at, igSHA(3), []leadstore.IGTouch{ok, bad}); !errors.Is(err, leadstore.ErrBadIG) {
			t.Errorf("%s: err = %v, want ErrBadIG", name, err)
		}
	}
	if _, err := f.q.InsertIGTouches(ctx, "not one", at, igSHA(3), []leadstore.IGTouch{ok}); !errors.Is(err, leadstore.ErrBadIG) {
		t.Errorf("vendor not a handle: err = %v, want ErrBadIG", err)
	}
	if _, err := f.q.InsertIGTouches(ctx, "vendor.example", at, []byte{1}, []leadstore.IGTouch{ok}); !errors.Is(err, leadstore.ErrBadIG) {
		t.Errorf("short hash: err = %v, want ErrBadIG", err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_vendor_touch`); n != 4 {
		t.Errorf("touches = %d, want 4 (a bad call stores nothing)", n)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_vendor_touch WHERE text IS NULL AND full_name IS NULL`); n != 3 {
		t.Errorf("touches with no text and no name = %d, want 3 (empty stores NULL)", n)
	}

	got, err := f.q.IGTouchesFor(ctx, []string{"cafe.exemplu", "fan.example", "nobody.example"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	c := got["cafe.exemplu"]
	if len(got) != 2 || len(c) != 3 || len(got["fan.example"]) != 1 {
		t.Fatalf("touches = %+v, want 3 of cafe.exemplu and 1 of fan.example", got)
	}
	want := leadstore.IGTouch{VendorHandle: "vendor.example", Handle: "cafe.exemplu", Kind: leadstore.IGTouchComment,
		Ref: "17900000000000001", PostCode: "PostA", At: day(20), Text: "Cum functioneaza cardul?", FullName: "Cafe Exemplu"}
	if c[0].At.Equal(want.At) {
		c[0].At = want.At
	}
	if c[0] != want || c[1].Kind != leadstore.IGTouchUsertag || c[2].Kind != leadstore.IGTouchTagged {
		t.Errorf("cafe.exemplu touches = %+v, want newest first starting %+v", c, want)
	}
	since, err := f.q.IGTouchesFor(ctx, []string{"cafe.exemplu"}, day(10))
	if err != nil {
		t.Fatal(err)
	}
	if s := since["cafe.exemplu"]; len(s) != 2 || s[1].Kind != leadstore.IGTouchUsertag {
		t.Errorf("since day 10 = %+v, want the comment and the usertag (on the day itself)", s)
	}
}
