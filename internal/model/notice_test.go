package model

import "testing"

func TestPrivacyNoticeURLAndBackupDays(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"", ""}, {" https://example.org/privacy ", "https://example.org/privacy"}, {"/privacy.html", "/privacy.html"},
		{"http://h:8080/p?x=1#a", "http://h:8080/p?x=1#a"},
		{"javascript:alert(1)", ""}, {"//evil.example/x", ""}, {"https://", ""}, {"https://u@h/", ""},
		{"https://h/a b", ""}, {"/p\"onmouseover", ""}, {"ftp://h/x", ""},
	} {
		a := DefaultAggregator()
		a.PrivacyNoticeURL = c.in
		n := a.Normalize()
		if a.PrivacyNoticeURL != c.want {
			t.Errorf("%q -> %q, want %q", c.in, a.PrivacyNoticeURL, c.want)
		}
		if (c.want == "" && c.in != "") != (len(n) > 0) {
			t.Errorf("%q: notes %v", c.in, n)
		}
	}
	a := DefaultAggregator()
	if a.Retention.BackupDays != 30 {
		t.Fatalf("backupDays default %d", a.Retention.BackupDays)
	}
	a.Retention.BackupDays = 0
	a.Normalize()
	if a.Retention.BackupDays != 30 {
		t.Fatalf("backupDays 0 -> %d", a.Retention.BackupDays)
	}
}
