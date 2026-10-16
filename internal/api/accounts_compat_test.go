package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Mastodon for iOS fetches accounts in batches whenever it loads a timeline
// or notifications; without this endpoint it shows "HTTP Error 405".
func TestAccountsBatch(t *testing.T) {
	f := newFixture(t)
	var accounts []map[string]interface{}
	path := "/api/v1/accounts?id[]=" + f.ids["bob"] + "&id[]=no-such-account&id[]=" + f.ids["alice"]
	if code := f.call(t, "alice", http.MethodGet, path, nil, &accounts); code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, code)
	}
	if len(accounts) != 2 || accounts[0]["username"] != "bob" || accounts[1]["username"] != "alice" {
		t.Errorf("accounts = %v, want bob then alice with the unknown ID left out", accounts)
	}
	if code := f.call(t, "", http.MethodGet, path, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous batch = %d, want 401", code)
	}
}

func TestAccountLookup(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	domain := "remote.example"
	remote := &models.Account{Username: "carol", Domain: &domain, ActorURI: "https://remote.example/users/carol",
		InboxURI: "https://remote.example/users/carol/inbox", PublicKeyPem: "pem", PublicKeyID: "https://remote.example/users/carol#main-key",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Approved: true}
	if err := f.store.CreateAccount(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[string]string{
		"bob":                   f.ids["bob"],
		"@bob":                  f.ids["bob"],
		"bob@test.example":      f.ids["bob"],
		"carol@remote.example":  remote.ID,
		"@carol@Remote.Example": remote.ID,
	} {
		var account map[string]interface{}
		if code := f.call(t, "", http.MethodGet, "/api/v1/accounts/lookup?acct="+acct, nil, &account); code != http.StatusOK || account["id"] != want {
			t.Errorf("lookup %q = %d %v, want %s", acct, code, account["id"], want)
		}
	}
	for _, acct := range []string{"nobody", "bob@elsewhere.example", ""} {
		if code := f.call(t, "", http.MethodGet, "/api/v1/accounts/lookup?acct="+acct, nil, nil); code == http.StatusOK {
			t.Errorf("lookup %q succeeded", acct)
		}
	}
}

func TestFamiliarFollowers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	carol := createAccount(t, f, "carol")
	// alice follows carol, and carol follows bob: carol is a familiar
	// follower of bob for alice.
	for _, pair := range [][2]string{{f.ids["alice"], carol}, {carol, f.ids["bob"]}} {
		if err := f.store.CreateFollow(ctx, &models.Follow{AccountID: pair[0], TargetAccountID: pair[1], URI: "https://test.example/follows/" + pair[0] + pair[1], State: models.FollowAccepted}); err != nil {
			t.Fatal(err)
		}
	}
	var result []map[string]interface{}
	if code := f.call(t, "alice", http.MethodGet, "/api/v1/accounts/familiar_followers?id[]="+f.ids["bob"], nil, &result); code != http.StatusOK {
		t.Fatalf("familiar_followers = %d", code)
	}
	if len(result) != 1 || result[0]["id"] != f.ids["bob"] {
		t.Fatalf("result = %v", result)
	}
	familiar := result[0]["accounts"].([]interface{})
	if len(familiar) != 1 || familiar[0].(map[string]interface{})["id"] != carol {
		t.Errorf("familiar followers of bob = %v, want carol", familiar)
	}
}

func createAccount(t *testing.T, f *fixture, username string) string {
	t.Helper()
	base := "https://test.example/users/" + username
	account := &models.Account{Username: username, ActorURI: base, InboxURI: base + "/inbox", PublicKeyPem: "pem",
		PublicKeyID: base + "#main-key", CreatedAt: time.Now(), UpdatedAt: time.Now(), Approved: true}
	if err := f.store.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account.ID
}
