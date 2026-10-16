package mediacache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const remote = "https://remote.example/files/cat.jpg"

type fakeStore struct {
	jobs       []*models.Job
	failed     bool
	media      []*models.MediaAttachment
	accounts   []*models.Account
	emojis     []*models.CustomEmoji
	updatedMed int
	updatedAcc int
	updatedEmo int
}

func (f *fakeStore) EnqueueJob(_ context.Context, job *models.Job) error {
	f.jobs = append(f.jobs, job)
	return nil
}
func (f *fakeStore) MediaCacheFailedSince(context.Context, string, time.Time) (bool, error) {
	return f.failed, nil
}
func (f *fakeStore) IsInstanceMediaCacheable(context.Context, string) (bool, error) { return true, nil }
func (f *fakeStore) GetMediaAttachmentsByRemoteURL(context.Context, string) ([]*models.MediaAttachment, error) {
	return f.media, nil
}
func (f *fakeStore) UpdateMediaAttachment(context.Context, *models.MediaAttachment) error {
	f.updatedMed++
	return nil
}
func (f *fakeStore) GetAccountsByImageURL(context.Context, string) ([]*models.Account, error) {
	return f.accounts, nil
}
func (f *fakeStore) UpdateAccount(context.Context, *models.Account) error {
	f.updatedAcc++
	return nil
}
func (f *fakeStore) GetEmojisByRemoteURL(context.Context, string) ([]*models.CustomEmoji, error) {
	return f.emojis, nil
}
func (f *fakeStore) UpdateEmoji(context.Context, *models.CustomEmoji) error {
	f.updatedEmo++
	return nil
}
func (f *fakeStore) RecordMediaCacheFailure(context.Context, string, string) error { return nil }
func (f *fakeStore) MarkInstanceUnreachable(context.Context, string) (bool, error) {
	return true, nil
}

type fakeCacher struct{ calls int }

func (c *fakeCacher) Store(context.Context, string) (*media.StoreResult, error) {
	c.calls++
	return &media.StoreResult{
		URL: "/media/cache/ab/hash/original.jpg", PreviewURL: "/media/cache/ab/hash/small.jpg",
		Blurhash: "LEHV6nWB2yk8", MIMEType: "image/jpeg", FileSize: 1234,
		StorageBackend: "local", StorageKey: "cache/ab/hash/original.jpg",
	}, nil
}

func TestEnqueue(t *testing.T) {
	store := &fakeStore{}
	queued, err := Enqueue(context.Background(), store, remote)
	if err != nil || !queued {
		t.Fatalf("Enqueue = %v, %v", queued, err)
	}
	if len(store.jobs) != 1 || store.jobs[0].Kind != JobKind {
		t.Fatalf("jobs = %+v", store.jobs)
	}
	var p Payload
	if err := json.Unmarshal([]byte(store.jobs[0].Payload), &p); err != nil || p.RemoteURL != remote {
		t.Errorf("payload = %q", store.jobs[0].Payload)
	}
}

func TestEnqueueSkipsRecentFailuresAndEmptyURLs(t *testing.T) {
	store := &fakeStore{failed: true}
	if queued, _ := Enqueue(context.Background(), store, remote); queued {
		t.Error("recently failed URL was queued again")
	}
	if queued, _ := Enqueue(context.Background(), store, ""); queued {
		t.Error("empty URL was queued")
	}
	if len(store.jobs) != 0 {
		t.Errorf("jobs = %d, want 0", len(store.jobs))
	}
}

func TestProcessRewritesReferencesToTheCachedCopy(t *testing.T) {
	attachment := &models.MediaAttachment{ID: "m1", URL: remote, RemoteURL: remote, PreviewURL: remote}
	account := &models.Account{ID: "a1", Avatar: remote, Header: "https://remote.example/other.jpg"}
	emoji := &models.CustomEmoji{ID: "e1", URL: remote}
	store := &fakeStore{
		media:    []*models.MediaAttachment{attachment},
		accounts: []*models.Account{account},
		emojis:   []*models.CustomEmoji{emoji},
	}
	cache := &fakeCacher{}
	payload, _ := json.Marshal(Payload{RemoteURL: remote})

	if err := Process(context.Background(), store, cache, string(payload)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if cache.calls != 1 {
		t.Errorf("cacher called %d times", cache.calls)
	}
	if attachment.URL != "/media/cache/ab/hash/original.jpg" || attachment.PreviewURL != "/media/cache/ab/hash/small.jpg" || attachment.CachedAt == nil {
		t.Errorf("attachment not rewritten: %+v", attachment)
	}
	if account.Avatar != "/media/cache/ab/hash/original.jpg" || account.AvatarRemote != remote {
		t.Errorf("avatar not rewritten: %+v", account)
	}
	if account.Header != "https://remote.example/other.jpg" {
		t.Error("an unrelated header was rewritten")
	}
	if emoji.URL != "/media/cache/ab/hash/original.jpg" || emoji.StaticURL != emoji.URL {
		t.Errorf("emoji not rewritten: %+v", emoji)
	}
	if store.updatedMed != 1 || store.updatedAcc != 1 || store.updatedEmo != 1 {
		t.Errorf("updates = media %d, accounts %d, emojis %d", store.updatedMed, store.updatedAcc, store.updatedEmo)
	}
}

func TestProcessWithNothingToCacheDoesNotDownload(t *testing.T) {
	cache := &fakeCacher{}
	payload, _ := json.Marshal(Payload{RemoteURL: remote})
	if err := Process(context.Background(), &fakeStore{}, cache, string(payload)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if cache.calls != 0 {
		t.Error("downloaded media nobody references")
	}
}
