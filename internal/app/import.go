package app

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogtheme"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"github.com/google/uuid"
)

// AccountImportMaxUploadSize bounds the accepted archive size. Generous:
// this is a full account (statuses + media), not a single upload.
const AccountImportMaxUploadSize = 4 << 30 // 4GiB

const maxAccountImportJSON = 16 << 20

var ErrAccountImportRateLimited = errors.New("account import is available once per hour")

// importNamespace roots the deterministic IDs assigned to imported statuses
// and media (see deterministicID). Deriving new IDs from the import ID plus
// the old item's stable identifier, rather than a random UUID, means a queue
// retry after a crash re-derives the exact same IDs: the existence checks in
// importStatuses/importMedia then recognize the row as already created and
// skip it instead of duplicating it.
var importNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("littlefedi-account-import"))

func deterministicID(importID, kind, oldKey string) string {
	return uuid.NewSHA1(importNamespace, []byte(importID+"|"+kind+"|"+oldKey)).String()
}

func accountImportKey(accountID, importID string) string {
	return "imports/" + accountID + "/" + importID + ".zip"
}

type accountImportPayload struct {
	ImportID  string `json:"import_id"`
	AccountID string `json:"account_id"`
}

// RequestAccountImport stages the uploaded archive in the object store and
// enqueues the background job that applies it. It mirrors RequestAccountExport:
// local accounts only, once per hour.
func RequestAccountImport(ctx context.Context, store storage.Storage, mediaStore objectstore.Store, account *models.Account, upload io.Reader, size int64) (*models.AccountImport, error) {
	if size <= 0 || size > AccountImportMaxUploadSize {
		return nil, fmt.Errorf("account archive size is invalid")
	}
	if account == nil || account.Domain != nil || account.IsServiceActor || account.DeletionState != "" {
		return nil, fmt.Errorf("account import is unavailable")
	}
	if mediaStore == nil {
		return nil, fmt.Errorf("account import media store is unavailable")
	}
	if latest, err := store.GetLatestAccountImport(ctx, account.ID); err == nil {
		if latest.State == "queued" || latest.State == "running" {
			return nil, fmt.Errorf("an account import is already in progress")
		}
		if latest.CreatedAt.After(time.Now().Add(-time.Hour)) {
			return nil, ErrAccountImportRateLimited
		}
	}
	imp := &models.AccountImport{AccountID: account.ID, State: "queued"}
	if err := store.CreateAccountImport(ctx, imp); err != nil {
		return nil, err
	}
	key := accountImportKey(account.ID, imp.ID)
	if _, err := mediaStore.Put(ctx, key, upload, size, objectstore.PutOptions{ContentType: "application/zip", CacheControl: "private, no-store"}); err != nil {
		_ = store.DeleteAccountImport(ctx, imp.ID, account.ID)
		return nil, err
	}
	payload, _ := json.Marshal(accountImportPayload{ImportID: imp.ID, AccountID: account.ID})
	if err := store.EnqueueJob(ctx, &models.Job{Kind: models.AccountImportJobKind, Payload: string(payload), MaxAttempts: 3, State: "queued"}); err != nil {
		_ = mediaStore.Delete(ctx, key)
		_ = store.DeleteAccountImport(ctx, imp.ID, account.ID)
		return nil, err
	}
	return imp, nil
}

// HandleAccountImportJob is the queue worker. Every step inside
// runAccountImport is best-effort (bad/unresolvable items are skipped and
// counted, not fatal); only archive-level problems (corrupt zip, wrong
// format, DB errors) fail the job and trigger a queue retry.
func HandleAccountImportJob(store storage.Storage, mediaStore objectstore.Store, federator *federation.Federator, domain string) func(context.Context, string) error {
	return func(ctx context.Context, body string) error {
		var payload accountImportPayload
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return fmt.Errorf("decode account import: %w", err)
		}
		imp, err := store.GetAccountImport(ctx, payload.ImportID, payload.AccountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if imp.State == "complete" {
			return nil
		}
		if mediaStore == nil {
			return fmt.Errorf("account import media store is unavailable")
		}
		account, err := store.GetAccount(ctx, payload.AccountID)
		if err != nil {
			return err
		}
		if err := store.StartAccountImport(ctx, imp.ID); err != nil {
			return err
		}
		key := accountImportKey(payload.AccountID, payload.ImportID)
		result, err := runAccountImport(ctx, store, mediaStore, federator, domain, imp.ID, account, key)
		if err != nil {
			_ = store.RecordAccountImportError(ctx, imp.ID, err.Error())
			return err
		}
		// Keep the staged archive on failure so the queue can retry it; daily
		// housekeeping removes abandoned import objects after seven days.
		_ = mediaStore.Delete(ctx, key)
		resultJSON, _ := json.Marshal(result)
		return store.CompleteAccountImport(ctx, imp.ID, string(resultJSON))
	}
}

func runAccountImport(ctx context.Context, store storage.Storage, mediaStore objectstore.Store, federator *federation.Federator, domain, importID string, account *models.Account, key string) (*models.AccountImportResult, error) {
	tmp, err := stageZipLocally(ctx, mediaStore, key)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > 100_000 {
		return nil, fmt.Errorf("account archive has too many entries")
	}

	files := make(map[string]*zip.File, len(zr.File))
	var expanded uint64
	for _, f := range zr.File {
		if f.UncompressedSize64 > AccountImportMaxUploadSize-expanded {
			return nil, fmt.Errorf("expanded account archive is too large")
		}
		expanded += f.UncompressedSize64
		if _, exists := files[f.Name]; exists {
			return nil, fmt.Errorf("duplicate archive entry %q", f.Name)
		}
		files[f.Name] = f
	}

	manifest := map[string]any{}
	if err := readZipJSON(files["manifest.json"], &manifest); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if format, _ := manifest["format"].(string); format != "littlefedi-account-export" {
		return nil, fmt.Errorf("unsupported archive format %q", manifest["format"])
	}

	var snapshot models.AccountExportSnapshot
	if err := readZipJSON(files["account.json"], &snapshot); err != nil {
		return nil, fmt.Errorf("read account.json: %w", err)
	}

	result := &models.AccountImportResult{}

	applyImportPreferences(account, snapshot.Preferences)
	if account.DisplayName == "" {
		account.DisplayName = snapshot.Profile.DisplayName
	}
	if account.Note == "" {
		account.Note = snapshot.Profile.Note
	}
	if err := store.UpdateAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("apply profile/preferences: %w", err)
	}

	for _, domainName := range snapshot.DomainBlocks {
		if domainName == "" {
			continue
		}
		if err := store.CreateAccountDomainBlock(ctx, account.ID, domainName); err != nil {
			slog.Warn("import: domain block failed", "domain", domainName, "error", err)
			result.DomainBlocks.Skipped++
			continue
		}
		result.DomainBlocks.Imported++
	}

	importBlocks(ctx, store, federator, account, snapshot.Blocks, &result.Blocks)
	importMutes(ctx, store, federator, account, snapshot.Mutes, &result.Mutes)
	importLists(ctx, store, federator, account, snapshot.Lists, &result.Lists)

	imported, skipped := ImportFollowHandles(ctx, store, federator, domain, account, snapshot.Following, 2*time.Second)
	result.Following = models.ImportCount{Imported: imported, Skipped: skipped}

	for _, name := range snapshot.FollowedHashtags {
		tag, err := store.GetOrCreateTag(ctx, name)
		if err != nil {
			slog.Warn("import: hashtag follow failed", "tag", name, "error", err)
			result.FollowedTags.Skipped++
			continue
		}
		if err := store.FollowTag(ctx, account.ID, tag.ID); err != nil {
			slog.Warn("import: hashtag follow failed", "tag", name, "error", err)
			result.FollowedTags.Skipped++
			continue
		}
		result.FollowedTags.Imported++
	}

	// Maps each old status's local ID to the deterministic ID its imported
	// copy will get. Media attaches to statuses by this old local ID (there's
	// no URI on MediaAttachment), so it's resolved and the media row created
	// with its final StatusID before the status itself exists - there's no FK
	// on media_attachments.status_id, so the ordering is safe.
	oldStatusIDToNewID := make(map[string]string, len(snapshot.Statuses))
	for _, old := range snapshot.Statuses {
		oldStatusIDToNewID[old.ID] = deterministicID(importID, "status", old.URI)
	}

	importMedia(ctx, store, mediaStore, files, oldStatusIDToNewID, importID, account.ID, snapshot.Media, &result.Media)

	if err := importStatuses(ctx, store, domain, importID, account, snapshot.Statuses, &result.Statuses); err != nil {
		return nil, err
	}

	for _, uri := range snapshot.Bookmarks {
		status, err := store.GetStatusByURI(ctx, uri)
		if err != nil || status == nil {
			result.Bookmarks.Skipped++
			continue
		}
		if existing, _ := store.GetBookmark(ctx, account.ID, status.ID); existing != nil {
			result.Bookmarks.Imported++
			continue
		}
		if err := store.CreateBookmark(ctx, &models.Bookmark{AccountID: account.ID, StatusID: status.ID}); err != nil {
			slog.Warn("import: bookmark failed", "uri", uri, "error", err)
			result.Bookmarks.Skipped++
			continue
		}
		result.Bookmarks.Imported++
	}

	result.FollowersNoted = len(snapshot.Followers)
	return result, nil
}

func stageZipLocally(ctx context.Context, mediaStore objectstore.Store, key string) (string, error) {
	reader, _, err := mediaStore.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	tmp, err := os.CreateTemp("", "littlefedi-import-*.zip")
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(tmp, io.LimitReader(reader, AccountImportMaxUploadSize+1))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil || n > AccountImportMaxUploadSize {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("stage account archive: %w", errors.Join(copyErr, closeErr, fmt.Errorf("archive size %d", n)))
	}
	return tmp.Name(), nil
}

func readZipJSON(f *zip.File, out any) error {
	if f == nil {
		return fmt.Errorf("missing archive entry")
	}
	if f.UncompressedSize64 > maxAccountImportJSON {
		return fmt.Errorf("archive JSON entry is too large")
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	limited := &io.LimitedReader{R: r, N: maxAccountImportJSON + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF || limited.N == 0 {
		return fmt.Errorf("archive JSON entry has trailing data or exceeds its size limit")
	}
	return nil
}

func applyImportPreferences(account *models.Account, prefs map[string]any) {
	if v, ok := prefs["default_post_language"].(string); ok {
		account.DefaultPostLanguage = v
	}
	if v, ok := prefs["default_post_markdown"].(bool); ok {
		account.DefaultPostMarkdown = v
	}
	if v, ok := prefs["default_post_visibility"].(string); ok {
		account.DefaultPostVisibility = models.Visibility(v)
	}
	if v, ok := prefs["hide_timeline_composer"].(bool); ok {
		account.HideTimelineComposer = v
	}
	if v, ok := prefs["locale"].(string); ok && v != "" {
		account.Locale = v
	}
	if v, ok := prefs["hide_replies"].(bool); ok {
		account.HideReplies = v
	}
	if v, ok := prefs["hide_boosts"].(bool); ok {
		account.HideBoosts = v
	}
	if v, ok := prefs["data_saver"].(bool); ok {
		account.DataSaver = v
	}
	if v, ok := prefs["hide_posts_without_alt"].(bool); ok {
		account.HidePostsWithoutAlt = v
	}
	if v, ok := prefs["hide_favourite_counts"].(bool); ok {
		account.HideFavouriteCounts = v
	}
	if v, ok := prefs["hide_boost_counts"].(bool); ok {
		account.HideBoostCounts = v
	}
	if v, ok := prefs["hide_follower_counts"].(bool); ok {
		account.HideFollowerCounts = v
	}
	if v, ok := prefs["hide_followers"].(bool); ok {
		account.HideFollowers = v
	}
	if v, ok := prefs["hide_following"].(bool); ok {
		account.HideFollowing = v
	}
	if v, ok := prefs["group_notifications"].(bool); ok {
		account.GroupNotifications = v
	}
	if v, ok := prefs["manually_approves_followers"].(bool); ok {
		account.ManuallyApprovesFollowers = v
	}
	if v, ok := prefs["quote_policy"].(string); ok && v != "" {
		account.QuotePolicy = v
	}
	if v, ok := prefs["discovery"].(bool); ok {
		account.Discovery = v
	}
	if v, ok := prefs["rss_feed_enabled"].(bool); ok {
		account.RSSFeedEnabled = v
	}
	if v, ok := prefs["blog_replies_enabled"].(bool); ok {
		account.BlogRepliesEnabled = v
	}
	// Empty means "follow the instance default", and the target instance may
	// not have the theme this archive was exported from - the web UI falls
	// back to its default when it does not recognise the value.
	if v, ok := prefs["theme"].(string); ok {
		account.Theme = v
	}
	if v, ok := prefs["appearance"].(string); ok {
		account.Appearance = v
	}
	// Re-validated rather than trusted: an archive is a file the user can edit
	// between export and import, and this value ends up inlined in a <style>.
	if v, ok := prefs["custom_css"].(string); ok {
		if css, err := content.ValidateCustomCSS(v); err == nil {
			account.CustomCSS = css
		}
	}
	// Same for the blog's own pair. An unknown blog theme costs nothing: the
	// generator falls back to the stock stylesheet.
	if v, ok := prefs["blog_theme"].(string); ok && blogtheme.IsSupported(v) {
		account.BlogTheme = v
	}
	if v, ok := prefs["blog_custom_css"].(string); ok {
		if css, err := content.ValidateCustomCSS(v); err == nil {
			account.BlogCustomCSS = css
		}
	}
}

func resolveImportHandle(ctx context.Context, store storage.Storage, federator *federation.Federator, handle string) (*models.Account, error) {
	handle = strings.TrimSpace(strings.TrimPrefix(handle, "@"))
	if handle == "" {
		return nil, fmt.Errorf("empty handle")
	}
	if federator != nil {
		return federator.ResolveAccount(ctx, handle)
	}
	if strings.Contains(handle, "@") {
		return nil, fmt.Errorf("cannot resolve remote handle without federation")
	}
	return store.GetAccountByUsernameDomain(ctx, handle, nil)
}

func importBlocks(ctx context.Context, store storage.Storage, federator *federation.Federator, account *models.Account, handles []string, count *models.ImportCount) {
	for _, handle := range handles {
		target, err := resolveImportHandle(ctx, store, federator, handle)
		if err != nil {
			slog.Warn("import: block resolve failed", "handle", handle, "error", err)
			count.Skipped++
			continue
		}
		if exists, _ := store.BlockExists(ctx, account.ID, target.ID); exists {
			count.Imported++
			continue
		}
		if err := store.CreateBlock(ctx, &models.Block{AccountID: account.ID, TargetAccountID: target.ID}); err != nil {
			slog.Warn("import: block create failed", "handle", handle, "error", err)
			count.Skipped++
			continue
		}
		count.Imported++
	}
}

func importMutes(ctx context.Context, store storage.Storage, federator *federation.Federator, account *models.Account, mutes []models.AccountExportMute, count *models.ImportCount) {
	for _, mute := range mutes {
		target, err := resolveImportHandle(ctx, store, federator, mute.Account)
		if err != nil {
			slog.Warn("import: mute resolve failed", "handle", mute.Account, "error", err)
			count.Skipped++
			continue
		}
		if muted, _ := store.IsMuted(ctx, account.ID, target.ID); muted {
			count.Imported++
			continue
		}
		m := &models.Mute{AccountID: account.ID, TargetAccountID: target.ID, HideNotifications: mute.HideNotifications, Duration: mute.DurationMinutes}
		if err := store.CreateMute(ctx, m); err != nil {
			slog.Warn("import: mute create failed", "handle", mute.Account, "error", err)
			count.Skipped++
			continue
		}
		count.Imported++
	}
}

func importLists(ctx context.Context, store storage.Storage, federator *federation.Federator, account *models.Account, lists []models.AccountExportList, count *models.ImportCount) {
	existing, err := store.ListLists(ctx, account.ID)
	if err != nil {
		slog.Warn("import: list existing lists failed", "error", err)
	}
	existingTitles := make(map[string]bool, len(existing))
	for _, l := range existing {
		existingTitles[l.Title] = true
	}
	for _, l := range lists {
		if existingTitles[l.Title] {
			count.Imported++
			continue
		}
		list := &models.List{AccountID: account.ID, Title: l.Title, RepliesPolicy: l.RepliesPolicy, Exclusive: l.Exclusive}
		if l.RepliesPolicy == "" {
			list.RepliesPolicy = models.ListRepliesList
		}
		if err := store.CreateList(ctx, list); err != nil {
			slog.Warn("import: list create failed", "title", l.Title, "error", err)
			count.Skipped++
			continue
		}
		for _, handle := range l.Members {
			member, err := resolveImportHandle(ctx, store, federator, handle)
			if err != nil {
				slog.Warn("import: list member resolve failed", "handle", handle, "error", err)
				continue
			}
			if err := store.AddAccountToList(ctx, list.ID, member.ID); err != nil {
				slog.Warn("import: list member add failed", "handle", handle, "error", err)
			}
		}
		existingTitles[l.Title] = true
		count.Imported++
	}
}

// importMedia re-uploads each attachment's bytes into this instance's own
// object store via the same helper normal uploads use (regenerating preview
// and blurhash), then creates the row directly with its final StatusID
// already resolved (via oldStatusIDToNewID) so the status-creation step
// doesn't need to touch media at all.
func importMedia(ctx context.Context, store storage.Storage, mediaStore objectstore.Store, files map[string]*zip.File, oldStatusIDToNewID map[string]string, importID, accountID string, media []*models.MediaAttachment, count *models.ImportCount) {
	for _, m := range media {
		if m == nil {
			continue
		}
		newID := deterministicID(importID, "media", m.ID)
		if existing, err := store.GetMediaAttachment(ctx, newID); err == nil && existing != nil {
			count.Imported++
			continue
		}

		entry := findMediaEntry(files, m.ID)
		if entry == nil {
			count.Skipped++
			continue
		}
		created, err := extractAndUpload(ctx, mediaStore, entry, newID, m)
		if err != nil {
			slog.Warn("import: media upload failed", "media_id", m.ID, "error", err)
			count.Skipped++
			continue
		}
		created.AccountID = accountID
		if m.StatusID != nil {
			if newStatusID, ok := oldStatusIDToNewID[*m.StatusID]; ok {
				created.StatusID = &newStatusID
			}
		}
		if err := store.CreateMediaAttachment(ctx, created); err != nil {
			slog.Warn("import: media create failed", "media_id", m.ID, "error", err)
			count.Skipped++
			continue
		}
		count.Imported++
	}
}

func findMediaEntry(files map[string]*zip.File, mediaID string) *zip.File {
	prefix := "media/" + mediaID + "/"
	for name, f := range files {
		if strings.HasPrefix(name, prefix) {
			return f
		}
	}
	return nil
}

func extractAndUpload(ctx context.Context, mediaStore objectstore.Store, entry *zip.File, newID string, orig *models.MediaAttachment) (*models.MediaAttachment, error) {
	if entry.UncompressedSize64 > AccountImportMaxUploadSize {
		return nil, fmt.Errorf("media entry is too large")
	}
	r, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	filename := path.Base(entry.Name)
	created := &models.MediaAttachment{
		ID:          newID,
		Type:        orig.Type,
		Description: orig.Description,
		Meta:        orig.Meta,
		MIMEType:    orig.MIMEType,
	}
	if err := medialib.StoreUpload(ctx, mediaStore, r, int64(entry.UncompressedSize64), created, filename, false); err != nil {
		return nil, err
	}
	return created, nil
}

// importStatuses recreates statuses via the storage layer directly (never
// the app-level publish path), so import never re-federates old posts to
// followers or re-notifies mentioned accounts. Reply/boost/quote targets are
// remapped by URI where resolvable and dropped (not fatal) otherwise. Media
// is already attached (see importMedia, called before this).
func importStatuses(ctx context.Context, store storage.Storage, domain, importID string, account *models.Account, statuses []*models.Status, count *models.ImportCount) error {
	// InReplyToURI/BoostOfURI/QuoteOfURI are always set alongside their *ID
	// counterpart when the referenced status is one this instance already
	// knew about (see PublishStatus and the BoostOfURI export enrichment), so
	// URI is enough: derive what this instance's own copy of that URI would
	// be named and check whether it exists, rather than tracking ID maps.
	resolve := func(oldURI *string) *string {
		if oldURI == nil || *oldURI == "" {
			return nil
		}
		candidate := deterministicID(importID, "status", *oldURI)
		if existing, err := store.GetStatus(ctx, candidate); err == nil && existing != nil {
			return &candidate
		}
		if found, err := store.GetStatusByURI(ctx, *oldURI); err == nil && found != nil {
			id := found.ID
			return &id
		}
		return nil
	}

	for _, old := range statuses {
		newID := deterministicID(importID, "status", old.URI)
		if existing, err := store.GetStatus(ctx, newID); err == nil && existing != nil {
			count.Imported++
			continue
		}

		if old.BoostOfID != nil {
			boostTarget := resolve(old.BoostOfURI)
			if boostTarget == nil {
				count.Skipped++
				continue
			}
			status := &models.Status{
				ID: newID, URI: fmt.Sprintf("https://%s/objects/%s", domain, newID),
				URL:       fmt.Sprintf("https://%s/@%s/%s", domain, account.Username, newID),
				AccountID: account.ID, BoostOfID: boostTarget,
				Visibility: old.Visibility, CreatedAt: old.CreatedAt, UpdatedAt: old.CreatedAt,
			}
			if err := store.CreateStatus(ctx, status); err != nil {
				slog.Warn("import: status (boost) create failed", "old_uri", old.URI, "error", err)
				count.Skipped++
				continue
			}
			count.Imported++
			continue
		}

		status := &models.Status{
			ID: newID, URI: fmt.Sprintf("https://%s/objects/%s", domain, newID),
			URL:           fmt.Sprintf("https://%s/@%s/%s", domain, account.Username, newID),
			AccountID:     account.ID,
			ContentHTML:   content.SanitizeHTMLStr(old.ContentHTML),
			ContentSource: old.ContentSource,
			ContentType:   old.ContentType,
			Summary:       old.Summary,
			Sensitive:     old.Sensitive,
			Visibility:    old.Visibility,
			Language:      old.Language,
			Pinned:        old.Pinned,
			CreatedAt:     old.CreatedAt,
			UpdatedAt:     old.CreatedAt,
			EditedAt:      old.EditedAt,
		}
		status.InReplyToID = resolve(old.InReplyToURI)
		status.InReplyToURI = old.InReplyToURI
		if quoteTarget := resolve(old.QuoteOfURI); quoteTarget != nil {
			status.QuoteOfID = quoteTarget
			status.QuoteOfURI = old.QuoteOfURI
			status.QuoteState = "accepted"
		}
		for _, name := range content.ExtractHashtags(old.ContentSource) {
			tag, err := store.GetOrCreateTag(ctx, name)
			if err != nil {
				continue
			}
			status.Tags = append(status.Tags, *tag)
		}
		if err := store.CreateStatus(ctx, status); err != nil {
			slog.Warn("import: status create failed", "old_uri", old.URI, "error", err)
			count.Skipped++
			continue
		}
		count.Imported++
	}
	return nil
}
