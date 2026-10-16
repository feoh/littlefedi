package migration

import (
	"errors"
	"reflect"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func TestAccountAliasesDeduplicates(t *testing.T) {
	account := &models.Account{AlsoKnownAs: "https://a.example/users/x, https://b.example/users/x\nhttps://A.example/users/x"}
	got := AccountAliases(account)
	want := []string{"https://a.example/users/x", "https://b.example/users/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AccountAliases = %v, want %v", got, want)
	}
	if AccountAliases(nil) != nil {
		t.Error("AccountAliases(nil) should be nil")
	}
}

func TestAppendAccountAlias(t *testing.T) {
	got := AppendAccountAlias("https://a.example/users/x", "https://b.example/users/x")
	if got != "https://a.example/users/x\nhttps://b.example/users/x" {
		t.Errorf("AppendAccountAlias = %q", got)
	}
	if again := AppendAccountAlias(got, "https://B.EXAMPLE/users/x"); again != got {
		t.Errorf("appending an existing alias changed the list: %q", again)
	}
}

func TestAccountAliasesForAP(t *testing.T) {
	if AccountAliasesForAP(&models.Account{}) != nil {
		t.Error("no aliases should give nil")
	}
	if got := AccountAliasesForAP(&models.Account{AlsoKnownAs: "https://a.example/u"}); got != "https://a.example/u" {
		t.Errorf("one alias should be a bare string, got %#v", got)
	}
	if got, ok := AccountAliasesForAP(&models.Account{AlsoKnownAs: "https://a.example/u https://b.example/u"}).([]string); !ok || len(got) != 2 {
		t.Errorf("two aliases should be a slice, got %#v", got)
	}
}

func TestSameActorURI(t *testing.T) {
	if !SameActorURI("https://Example.COM/users/x", "https://example.com/users/x") {
		t.Error("scheme and host comparison should be case-insensitive")
	}
	if SameActorURI("https://example.com/users/X", "https://example.com/users/x") {
		t.Error("path comparison should be case-sensitive")
	}
	if SameActorURI("", "") {
		t.Error("empty URIs should never match")
	}
}

func TestVerifyMovePair(t *testing.T) {
	oldAcct := &models.Account{ActorURI: "https://old.example/users/me", MovedToURI: "https://new.example/users/me"}
	newAcct := &models.Account{ActorURI: "https://new.example/users/me", AlsoKnownAs: "https://old.example/users/me"}
	if err := VerifyMovePair(oldAcct, newAcct, true); err != nil {
		t.Fatalf("valid move rejected: %v", err)
	}

	if err := VerifyMovePair(oldAcct, &models.Account{ActorURI: newAcct.ActorURI}, false); !errors.Is(err, ErrTargetAliasMissing) {
		t.Errorf("missing alias error = %v", err)
	}
	stale := &models.Account{ActorURI: oldAcct.ActorURI}
	if err := VerifyMovePair(stale, newAcct, true); !errors.Is(err, ErrOldMovedToMismatch) {
		t.Errorf("movedTo mismatch error = %v", err)
	}
	if err := VerifyMovePair(stale, newAcct, false); err != nil {
		t.Errorf("movedTo should not be required: %v", err)
	}
}
