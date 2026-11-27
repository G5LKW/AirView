package config

import "testing"

// TestShareLinksBlock: aggregator.shareLinks from the environment and -set,
// the range checks and the defaultDays <= maxDays cross-check.
func TestShareLinksBlock(t *testing.T) {
	dir := t.TempDir()
	r := Check(LoadOptions{Dir: dir, NoFile: true, Environ: []string{"AIRVIEW_MODE=aggregator",
		"AIRVIEW_AGGREGATOR_SHARE_LINKS_DISABLED=true", "AIRVIEW_AGGREGATOR_SHARE_LINKS_MAX_PER_OWNER=5"}})
	c := r.Effective.Aggregator.ShareLinks
	if !c.Disabled || c.MaxPerOwner != 5 {
		t.Fatalf("shareLinks %+v\n%s", c, issuesText(r.Issues))
	}
	r = Check(LoadOptions{Dir: dir, NoFile: true, Sets: []string{"mode=aggregator",
		"aggregator.shareLinks.maxDays=10", "aggregator.shareLinks.defaultDays=20", "aggregator.shareLinks.maxPerOwner=5000"}})
	c = r.Effective.Aggregator.ShareLinks
	if !issueAt(r, "aggregator.shareLinks.defaultDays") || c.DefaultDays != 10 {
		t.Fatalf("defaultDays above maxDays not reported: %+v\n%s", c, issuesText(r.Issues))
	}
	if !issueAt(r, "aggregator.shareLinks.maxPerOwner") || c.MaxPerOwner != 0 {
		t.Fatalf("maxPerOwner out of range not reported: %+v\n%s", c, issuesText(r.Issues))
	}
	mp, md, dd, coarse := c.Effective()
	if mp != 20 || md != 10 || dd != 10 || !coarse {
		t.Fatalf("Effective = %d %d %d %v", mp, md, dd, coarse)
	}
}
