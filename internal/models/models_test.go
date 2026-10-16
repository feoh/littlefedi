package models

import (
	"testing"
	"time"
)

func TestCanAuthenticate(t *testing.T) {
	ok := Account{Approved: true}
	if !ok.CanAuthenticate(false) {
		t.Fatal("an approved local account should authenticate")
	}
	domain := "remote.example"
	now := time.Now()
	blocked := map[string]Account{
		"remote":       {Approved: true, Domain: &domain},
		"service":      {Approved: true, IsServiceActor: true},
		"unapproved":   {},
		"suspended":    {Approved: true, SuspendedAt: &now},
		"disabled":     {Approved: true, ModerationState: "disabled"},
		"deleting":     {Approved: true, DeletionState: "pending"},
		"unverified":   {Approved: true},
		"admin no 2fa": {Approved: true, IsAdmin: true},
	}
	for name, account := range blocked {
		requireEmail := name == "unverified"
		requireMFA := name == "admin no 2fa"
		if account.CanAuthenticate(requireEmail, requireMFA) {
			t.Errorf("%s account was allowed to authenticate", name)
		}
	}
	if (&Account{Approved: true, IsAdmin: true, MFAEnabled: true}).CanAuthenticate(false, true) == false {
		t.Error("admin with MFA should authenticate when MFA is required")
	}
	var nilAccount *Account
	if nilAccount.CanAuthenticate(false) {
		t.Error("nil account authenticated")
	}
}

func TestMediaDimensions(t *testing.T) {
	if w, h := (MediaAttachment{Meta: `{"width":640,"height":480}`}).Dimensions(); w != 640 || h != 480 {
		t.Errorf("Dimensions = %dx%d", w, h)
	}
	for _, meta := range []string{"", "not json", `{"width":0,"height":10}`} {
		if w, h := (MediaAttachment{Meta: meta}).Dimensions(); w != 0 || h != 0 {
			t.Errorf("Dimensions(%q) = %dx%d, want 0x0", meta, w, h)
		}
	}
}

func TestParseFollowImportNotificationType(t *testing.T) {
	if kind, id := ParseFollowImportNotificationType("follow_import.complete:abc"); kind != "follow_import.complete" || id != "abc" {
		t.Errorf("got %q %q", kind, id)
	}
	for _, bad := range []string{"follow_import.complete", "mention:abc", ""} {
		if kind, id := ParseFollowImportNotificationType(bad); kind != "" || id != "" {
			t.Errorf("ParseFollowImportNotificationType(%q) = %q %q", bad, kind, id)
		}
	}
}

func TestAllowsRelationshipNotification(t *testing.T) {
	quiet := &Account{}
	loud := &Account{NotifySensitiveRelationships: true}
	if !quiet.AllowsRelationshipNotification("follow", false) {
		t.Error("ordinary notifications should always be allowed")
	}
	if quiet.AllowsRelationshipNotification("unfollow", false) {
		t.Error("sensitive notification allowed without opt-in")
	}
	if !loud.AllowsRelationshipNotification("block", false) || loud.AllowsRelationshipNotification("block", true) {
		t.Error("opt-in sensitive notifications should respect blocks")
	}
}
