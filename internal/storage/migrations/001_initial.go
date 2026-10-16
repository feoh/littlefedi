package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Initial schema for new databases.
func init() {
	Migrations.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			isSQLite := db.Dialect().Name().String() == "sqlite"
			if isSQLite {
				for _, pragma := range []string{
					"PRAGMA journal_mode=WAL",
					"PRAGMA foreign_keys=ON",
					"PRAGMA busy_timeout=10000",
				} {
					if _, err := db.ExecContext(ctx, pragma); err != nil {
						return fmt.Errorf("pragma: %w", err)
					}
				}
			}

			return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				for _, statement := range initialSchemaTables {
					if _, err := tx.ExecContext(ctx, statement); err != nil {
						return fmt.Errorf("create table: %w", err)
					}
				}
				indexes := initialPostgresIndexes
				if isSQLite {
					indexes = initialSQLiteIndexes
				}
				for _, statements := range [][]string{initialSchemaIndexes, indexes} {
					for _, statement := range statements {
						if _, err := tx.ExecContext(ctx, statement); err != nil {
							return fmt.Errorf("create index: %w", err)
						}
					}
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO federation_policy_state(id, mode, commercial_bigtech_enabled)
VALUES (1, 'open', FALSE) ON CONFLICT (id) DO NOTHING`)
				return err
			})
		},
		func(ctx context.Context, db *bun.DB) error {
			return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				for _, table := range []string{
					"welcome_direct_messages",
					"mesh_identity",
					"instance_meta",
					"federation_policy_state",
					"featured_tags",
					"domain_allows",
					"commercial_domain_blocks",
					"status_previews",
					"vapid_keys",
					"tombstones",
					"tags",
					"statuses",
					"status_tags",
					"sessions",
					"self_destruct_runs",
					"scheduled_statuses",
					"reports",
					"relays",
					"quote_authorizations",
					"push_subscriptions",
					"polls",
					"poll_votes",
					"poll_options",
					"password_resets",
					"oauth_tokens",
					"oauth_applications",
					"notifications",
					"mutes",
					"moderation_logs",
					"mfa_recovery_codes",
					"mentions",
					"media_storage_migrations",
					"media_cache_failures",
					"media_attachments",
					"markers",
					"lists",
					"list_accounts",
					"jobs",
					"job_payloads",
					"invites",
					"instances",
					"home_feed",
					"follows",
					"followed_tags",
					"filters",
					"filter_keywords",
					"emoji_reactions",
					"favourites",
					"email_verifications",
					"email_digest_state",
					"domain_blocks",
					"custom_emojis",
					"bubble_instances",
					"bookmarks",
					"blocks",
					"announcements",
					"accounts",
					"account_imports",
					"account_exports",
					"account_domain_blocks",
					"account_deletion_runs",
					"account_deletion_destinations",
				} {
					if _, err := tx.NewDropTable().Table(table).IfExists().Exec(ctx); err != nil {
						return fmt.Errorf("drop table %s: %w", table, err)
					}
				}
				return nil
			})
		},
	)
}

var initialSchemaTables = []string{
	`CREATE TABLE IF NOT EXISTS account_deletion_destinations(
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  inbox TEXT NOT NULL,
  delivered_at TIMESTAMP,
  UNIQUE(run_id, inbox)
);`,

	`CREATE TABLE IF NOT EXISTS account_deletion_runs(
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL UNIQUE,
  actor_uri TEXT NOT NULL,
  requested_at TIMESTAMP NOT NULL,
  deadline_at TIMESTAMP NOT NULL,
  completed_at TIMESTAMP,
  state TEXT NOT NULL,
  destination_count INTEGER NOT NULL,
  delivered_count INTEGER NOT NULL DEFAULT 0
);`,

	`CREATE TABLE IF NOT EXISTS "account_domain_blocks"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "domain" VARCHAR NOT NULL,
  PRIMARY KEY("id"),
  CONSTRAINT "idx_acct_domain_block" UNIQUE("account_id", "domain")
);`,

	`CREATE TABLE IF NOT EXISTS account_exports(
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  state TEXT NOT NULL,
  storage_key TEXT NOT NULL DEFAULT '',
  size BIGINT NOT NULL DEFAULT 0,
  token_hash TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMP,
  downloaded_at TIMESTAMP,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);`,

	`CREATE TABLE IF NOT EXISTS account_imports(
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  state TEXT NOT NULL,
  result_json TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS "accounts"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "username" VARCHAR NOT NULL,
  "domain" VARCHAR,
  "actor_uri" VARCHAR NOT NULL,
  "inbox_uri" VARCHAR,
  "shared_inbox_uri" VARCHAR,
  "outbox_uri" VARCHAR,
  "followers_uri" VARCHAR,
  "following_uri" VARCHAR,
  "public_key_pem" VARCHAR NOT NULL,
  "private_key_pem" VARCHAR,
  "display_name" VARCHAR,
  "note" VARCHAR,
  "note_source" VARCHAR,
  "avatar" VARCHAR,
  "avatar_remote" VARCHAR,
  "header" VARCHAR,
  "header_remote" VARCHAR,
  "manually_approves_followers" BOOLEAN NOT NULL DEFAULT false,
  "locked" BOOLEAN NOT NULL DEFAULT false,
  "suspended_at" timestamp,
  "fetched_at" timestamp,
  "email" VARCHAR,
  "password_hash" VARCHAR,
  "followers_count" INTEGER NOT NULL DEFAULT 0,
  "following_count" INTEGER NOT NULL DEFAULT 0,
  "statuses_count" INTEGER NOT NULL DEFAULT 0,
  "also_known_as" VARCHAR,
  "moved_to_uri" VARCHAR,
  "discovery" BOOLEAN NOT NULL DEFAULT true,
  is_admin boolean NOT NULL DEFAULT false,
  approved boolean NOT NULL DEFAULT true,
  approved_at timestamp,
  invite_id text,
  prune_own_statuses_after_days INTEGER,
  prune_own_statuses_min_likes INTEGER,
  prune_own_statuses_min_boosts INTEGER,
  hide_followers BOOLEAN NOT NULL DEFAULT FALSE,
  hide_following BOOLEAN NOT NULL DEFAULT FALSE,
  moved_at TIMESTAMP,
  hide_replies BOOLEAN NOT NULL DEFAULT FALSE,
  hide_boosts BOOLEAN NOT NULL DEFAULT FALSE,
  quote_policy TEXT NOT NULL DEFAULT 'everyone',
  moderation_state TEXT NOT NULL DEFAULT '',
  moderation_note TEXT NOT NULL DEFAULT '',
  group_notifications BOOLEAN NOT NULL DEFAULT FALSE,
  notification_excluded_types TEXT NOT NULL DEFAULT '',
  is_service_actor BOOLEAN NOT NULL DEFAULT FALSE,
  actor_next_refresh_at TIMESTAMP NULL,
  actor_refresh_lease_until TIMESTAMP NULL,
  actor_refresh_failures INTEGER NOT NULL DEFAULT 0,
  counts_fetched_at TIMESTAMP NULL,
  email_verified BOOLEAN NOT NULL DEFAULT false,
  email_verified_at TIMESTAMPTZ,
  email_notify_mention BOOLEAN NOT NULL DEFAULT true,
  email_notify_follow BOOLEAN NOT NULL DEFAULT true,
  email_notify_reblog BOOLEAN NOT NULL DEFAULT false,
  email_notify_favourite BOOLEAN NOT NULL DEFAULT false,
  email_notify_poll BOOLEAN NOT NULL DEFAULT false,
  email_notify_follow_request BOOLEAN NOT NULL DEFAULT true,
  email_notify_quote BOOLEAN NOT NULL DEFAULT true,
  email_notify_report BOOLEAN NOT NULL DEFAULT true,
  email_notify_approval BOOLEAN NOT NULL DEFAULT true,
  email_notify_digest BOOLEAN NOT NULL DEFAULT false,
  email_notify_backup BOOLEAN NOT NULL DEFAULT true,
  email_notify_instance BOOLEAN NOT NULL DEFAULT true,
  email_notify_backup_success BOOLEAN NOT NULL DEFAULT true,
  email_notify_backup_failure BOOLEAN NOT NULL DEFAULT true,
  locale TEXT NOT NULL DEFAULT 'en',
  default_post_language TEXT NOT NULL DEFAULT 'en',
  deletion_state TEXT NOT NULL DEFAULT '',
  deletion_requested_at TIMESTAMP,
  mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  totp_secret_encrypted TEXT NOT NULL DEFAULT '',
  "blog_allowed" BOOLEAN NOT NULL DEFAULT FALSE,
  is_bot BOOLEAN NOT NULL DEFAULT false,
  "email_notify_blog_rebuild" BOOLEAN NOT NULL DEFAULT FALSE,
  "email_notify_account_activity" BOOLEAN NOT NULL DEFAULT FALSE,
  "blog_slug_format" TEXT NOT NULL DEFAULT '',
  "public_key_id" TEXT NOT NULL DEFAULT '',
  "ed25519_public_key_pem" TEXT NOT NULL DEFAULT '',
  "ed25519_public_key_id" TEXT NOT NULL DEFAULT '',
  "ed25519_private_key_pem" TEXT NOT NULL DEFAULT '',
  "public_keys_json" TEXT NOT NULL DEFAULT '{}',
  "email_notify_new_account" BOOLEAN NOT NULL DEFAULT FALSE,
  "email_notify_invite_signup" BOOLEAN NOT NULL DEFAULT FALSE,
  "default_post_markdown" BOOLEAN NOT NULL DEFAULT FALSE,
  "default_post_visibility" TEXT NOT NULL DEFAULT 'public',
  "rss_feed_enabled" BOOLEAN NOT NULL DEFAULT FALSE,
  "blog_replies_enabled" BOOLEAN NOT NULL DEFAULT TRUE,
  "theme" TEXT NOT NULL DEFAULT '',
  "appearance" TEXT NOT NULL DEFAULT '',
  "custom_css" TEXT NOT NULL DEFAULT '',
  "blog_theme" TEXT NOT NULL DEFAULT '',
  "blog_custom_css" TEXT NOT NULL DEFAULT '',
  first_seen_at TIMESTAMP,
  "email_notify_account_departure" BOOLEAN NOT NULL DEFAULT FALSE,
  "hide_timeline_composer" BOOLEAN NOT NULL DEFAULT FALSE,
  "data_saver" BOOLEAN NOT NULL DEFAULT FALSE,
  "hide_posts_without_alt" BOOLEAN NOT NULL DEFAULT FALSE,
  "notify_sensitive_relationships" BOOLEAN NOT NULL DEFAULT FALSE,
  "hide_favourite_counts" BOOLEAN NOT NULL DEFAULT FALSE,
  "hide_boost_counts" BOOLEAN NOT NULL DEFAULT FALSE,
  "hide_follower_counts" BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY("id"),
  UNIQUE("actor_uri")
);`,

	`CREATE TABLE IF NOT EXISTS "announcements"(
  "id" text NOT NULL,
  "content" VARCHAR NOT NULL,
  "starts_at" timestamp,
  "ends_at" timestamp,
  "all_day" BOOLEAN NOT NULL DEFAULT false,
  "published_at" timestamp NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "blocks"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "target_account_id" VARCHAR NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "bookmarks"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "status_id" VARCHAR NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "bubble_instances"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "domain" VARCHAR NOT NULL,
  "note" VARCHAR,
  PRIMARY KEY("id"),
  UNIQUE("domain")
);`,

	`CREATE TABLE IF NOT EXISTS "custom_emojis"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "shortcode" VARCHAR NOT NULL,
  "domain" VARCHAR,
  "url" VARCHAR NOT NULL,
  "static_url" VARCHAR,
  "remote_url" VARCHAR,
  "visible_in_picker" BOOLEAN NOT NULL DEFAULT true,
  "cached_at" timestamp,
  PRIMARY KEY("id"),
  CONSTRAINT "idx_emoji_shortcode_domain" UNIQUE("shortcode", "domain")
);`,

	`CREATE TABLE IF NOT EXISTS "domain_blocks"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "domain" VARCHAR NOT NULL,
  "severity" VARCHAR NOT NULL DEFAULT 'silence',
  "public" BOOLEAN NOT NULL DEFAULT false,
  "comment" VARCHAR,
  reject_media BOOLEAN NOT NULL DEFAULT FALSE,
  reject_reports BOOLEAN NOT NULL DEFAULT FALSE,
  obfuscate BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY("id"),
  UNIQUE("domain")
);`,

	`CREATE TABLE IF NOT EXISTS "email_digest_state"(
  "account_id" text NOT NULL,
  "last_sent_at" timestamp NOT NULL,
  PRIMARY KEY("account_id")
);`,

	`CREATE TABLE IF NOT EXISTS "email_verifications"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "expires_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "email" VARCHAR NOT NULL,
  "token_hash" VARCHAR NOT NULL,
  "used" BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "favourites"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "status_id" VARCHAR NOT NULL,
  "uri" VARCHAR,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS emoji_reactions(
  id TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL,
  uri TEXT NOT NULL UNIQUE,
  account_id TEXT NOT NULL,
  status_id TEXT NOT NULL,
  content TEXT NOT NULL,
  reaction_key TEXT NOT NULL,
  emoji_id TEXT,
  UNIQUE(account_id, status_id, reaction_key)
);`,

	`CREATE TABLE IF NOT EXISTS "filter_keywords"(
  "id" text NOT NULL,
  "filter_id" VARCHAR NOT NULL,
  "keyword" VARCHAR NOT NULL,
  "whole_word" BOOLEAN NOT NULL DEFAULT true,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "filters"(
  "id" text NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "title" VARCHAR NOT NULL,
  "context_json" VARCHAR NOT NULL,
  "filter_action" VARCHAR NOT NULL DEFAULT 'warn',
  "expires_at" timestamp,
  "languages_json" VARCHAR NOT NULL DEFAULT '[]',
  "invert_languages" BOOLEAN NOT NULL DEFAULT FALSE,
  "show_unknown_languages" BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "followed_tags"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "tag_id" VARCHAR NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "follows"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "target_account_id" VARCHAR NOT NULL,
  "uri" VARCHAR NOT NULL,
  "state" VARCHAR NOT NULL DEFAULT 'accepted',
  "show_reblogs" BOOLEAN NOT NULL DEFAULT true,
  "notify" BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY("id"),
  UNIQUE("uri")
);`,

	`CREATE TABLE IF NOT EXISTS home_feed(
  account_id TEXT NOT NULL,
  status_id TEXT NOT NULL,
  author_account_id TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  PRIMARY KEY(account_id, status_id)
);`,

	`CREATE TABLE IF NOT EXISTS "instances"(
  "id" text NOT NULL,
  "domain" VARCHAR NOT NULL,
  "software" VARCHAR,
  "version" VARCHAR,
  "nodeinfo_url" VARCHAR,
  "unreachable_since" timestamp,
  "backoff_count" INTEGER NOT NULL DEFAULT 0,
  "last_success" timestamp,
  "last_failure" timestamp,
  "suspended" BOOLEAN NOT NULL DEFAULT false,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  PRIMARY KEY("id"),
  UNIQUE("domain")
);`,

	`CREATE TABLE IF NOT EXISTS "invites"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "expires_at" timestamp,
  "max_uses" INTEGER NOT NULL DEFAULT 0,
  "use_count" INTEGER NOT NULL DEFAULT 0,
  "created_by_id" VARCHAR NOT NULL,
  "code" VARCHAR NOT NULL,
  PRIMARY KEY("id"),
  UNIQUE("code")
);`,

	`CREATE TABLE IF NOT EXISTS job_payloads(
  id TEXT PRIMARY KEY,
  body TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS "jobs"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "kind" VARCHAR NOT NULL,
  "payload" VARCHAR NOT NULL,
  "run_at" VARCHAR NOT NULL,
  "attempts" INTEGER NOT NULL,
  "max_attempts" INTEGER NOT NULL,
  "state" VARCHAR NOT NULL,
  "error" VARCHAR,
  "updated_at" timestamp NOT NULL,
  "taken_at" timestamp,
  "priority" INTEGER NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "list_accounts"(
  "list_id" VARCHAR NOT NULL,
  "account_id" VARCHAR NOT NULL,
  PRIMARY KEY("list_id", "account_id")
);`,

	`CREATE TABLE IF NOT EXISTS "lists"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "title" VARCHAR NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "replies_policy" VARCHAR NOT NULL DEFAULT 'list',
  "exclusive" BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS markers(
  account_id TEXT NOT NULL,
  timeline TEXT NOT NULL,
  last_read_id TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 0,
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY(account_id, timeline)
);`,

	`CREATE TABLE IF NOT EXISTS "media_attachments"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "status_id" VARCHAR,
  "account_id" VARCHAR NOT NULL,
  "type" VARCHAR NOT NULL,
  "url" VARCHAR,
  "preview_url" VARCHAR,
  "remote_url" VARCHAR,
  "blurhash" VARCHAR,
  "description" VARCHAR,
  "meta" VARCHAR,
  "file_size" INTEGER,
  "mime_type" VARCHAR,
  cached_at TIMESTAMP,
  storage_backend text NOT NULL DEFAULT 'local',
  storage_key text NOT NULL DEFAULT '',
  preview_storage_key text NOT NULL DEFAULT '',
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "media_cache_failures"(
  "url" text NOT NULL,
  "reason" VARCHAR NOT NULL,
  "failed_at" timestamp NOT NULL,
  PRIMARY KEY("url")
);`,

	`CREATE TABLE IF NOT EXISTS media_storage_migrations(
  id text PRIMARY KEY,
  created_at timestamp NOT NULL,
  updated_at timestamp NOT NULL,
  source_config text NOT NULL,
  destination_config text NOT NULL,
  state text NOT NULL,
  cursor text NOT NULL DEFAULT '',
  total bigint NOT NULL DEFAULT 0,
  copied bigint NOT NULL DEFAULT 0,
  failed bigint NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  cancel_requested boolean NOT NULL DEFAULT false
);`,

	`CREATE TABLE IF NOT EXISTS "mentions"(
  "id" text NOT NULL,
  "status_id" VARCHAR NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "username" VARCHAR NOT NULL,
  "url" VARCHAR NOT NULL,
  "acct" VARCHAR NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS mfa_recovery_codes(
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  code_hash TEXT NOT NULL UNIQUE,
  created_at TIMESTAMP NOT NULL,
  used_at TIMESTAMP
);`,

	`CREATE TABLE IF NOT EXISTS "moderation_logs"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "admin_account_id" VARCHAR NOT NULL,
  "target_type" VARCHAR NOT NULL,
  "target_id" VARCHAR NOT NULL,
  "action" VARCHAR NOT NULL,
  "note" VARCHAR,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "mutes"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "target_account_id" VARCHAR NOT NULL,
  "hide_notifications" BOOLEAN NOT NULL DEFAULT false,
  "duration" INTEGER,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "notifications"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "type" VARCHAR NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "from_account_id" VARCHAR NOT NULL,
  "status_id" VARCHAR,
  "emoji" VARCHAR,
  "read" BOOLEAN NOT NULL DEFAULT false,
  api_id BIGINT,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "oauth_applications"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "name" VARCHAR NOT NULL,
  "website" VARCHAR,
  "redirect_uris" VARCHAR NOT NULL,
  "client_id" VARCHAR NOT NULL,
  "client_secret" VARCHAR NOT NULL,
  "scopes" VARCHAR NOT NULL DEFAULT 'read',
  PRIMARY KEY("id"),
  UNIQUE("client_id")
);`,

	`CREATE TABLE IF NOT EXISTS "oauth_tokens"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "access_token" VARCHAR NOT NULL,
  "refresh_token" VARCHAR,
  "account_id" VARCHAR NOT NULL,
  "application_id" VARCHAR NOT NULL,
  "scopes" VARCHAR NOT NULL,
  "expires_at" timestamp,
  "revoked_at" timestamp,
  authorization_code BOOLEAN NOT NULL DEFAULT FALSE,
  redirect_uri TEXT NOT NULL DEFAULT '',
  code_challenge TEXT NOT NULL DEFAULT '',
  code_challenge_method TEXT NOT NULL DEFAULT '',
  PRIMARY KEY("id"),
  UNIQUE("access_token"),
  UNIQUE("refresh_token")
);`,

	`CREATE TABLE IF NOT EXISTS "password_resets"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "expires_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "token_hash" VARCHAR NOT NULL,
  "used" BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY("id"),
  UNIQUE("token_hash")
);`,

	`CREATE TABLE IF NOT EXISTS "poll_options"(
  "id" text NOT NULL,
  "poll_id" VARCHAR NOT NULL,
  "position" INTEGER NOT NULL,
  "title" VARCHAR NOT NULL,
  "votes_count" INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "poll_votes"(
  "id" text NOT NULL,
  "poll_id" VARCHAR NOT NULL,
  "option_id" VARCHAR NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "created_at" timestamp NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "polls"(
  "id" text NOT NULL,
  "status_id" VARCHAR NOT NULL,
  "expires_at" timestamp,
  "expired" BOOLEAN NOT NULL DEFAULT false,
  "multiple" BOOLEAN NOT NULL DEFAULT false,
  "hide_totals" BOOLEAN NOT NULL DEFAULT false,
  "votes_count" INTEGER NOT NULL DEFAULT 0,
  "voters_count" INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY("id"),
  UNIQUE("status_id")
);`,

	`CREATE TABLE IF NOT EXISTS "push_subscriptions"(
  id TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  account_id TEXT NOT NULL,
  oauth_token_id TEXT,
  session_id TEXT,
  label TEXT,
  endpoint TEXT NOT NULL,
  p256dh TEXT NOT NULL,
  auth TEXT NOT NULL,
  notify_mention BOOLEAN NOT NULL DEFAULT true,
  notify_follow BOOLEAN NOT NULL DEFAULT false,
  notify_reblog BOOLEAN NOT NULL DEFAULT false,
  notify_favourite BOOLEAN NOT NULL DEFAULT false,
  notify_poll BOOLEAN NOT NULL DEFAULT true,
  notify_follow_request BOOLEAN NOT NULL DEFAULT false,
  notify_status BOOLEAN NOT NULL DEFAULT true,
  notify_quote BOOLEAN NOT NULL DEFAULT true,
  policy TEXT NOT NULL DEFAULT 'all',
  "notify_blog_rebuild" BOOLEAN NOT NULL DEFAULT TRUE,
  CHECK((oauth_token_id IS NOT NULL AND session_id IS NULL) OR(oauth_token_id IS NULL AND session_id IS NOT NULL))
);`,

	`CREATE TABLE IF NOT EXISTS "quote_authorizations"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "uri" VARCHAR NOT NULL,
  "quoted_status_id" VARCHAR NOT NULL,
  "quoting_uri" VARCHAR NOT NULL,
  "quoting_actor_uri" VARCHAR NOT NULL,
  PRIMARY KEY("id"),
  UNIQUE("uri")
);`,

	`CREATE TABLE IF NOT EXISTS relays(
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL UNIQUE,
  follow_id TEXT,
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS "reports"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "account_id" VARCHAR,
  "target_account_id" VARCHAR NOT NULL,
  "status_ids" VARCHAR,
  "comment" VARCHAR,
  "category" VARCHAR NOT NULL DEFAULT 'other',
  "forwarded" BOOLEAN NOT NULL DEFAULT false,
  "state" VARCHAR NOT NULL DEFAULT 'open',
  "assigned_admin_id" VARCHAR,
  "action_taken" VARCHAR,
  "origin_domain" VARCHAR,
  "resolved_at" timestamp,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS "scheduled_statuses"(
  "id" text NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "params_json" VARCHAR NOT NULL,
  "scheduled_at" timestamp NOT NULL,
  "created_at" timestamp NOT NULL,
  PRIMARY KEY("id")
);`,

	`CREATE TABLE IF NOT EXISTS self_destruct_runs(
  id TEXT PRIMARY KEY,
  started_at TIMESTAMP NOT NULL,
  identity_count INTEGER NOT NULL,
  inbox_count INTEGER NOT NULL,
  delivery_count INTEGER NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS "sessions"(
  "id" text NOT NULL,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "expires_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "token_hash" VARCHAR NOT NULL,
  "user_agent" VARCHAR,
  "ip_address" VARCHAR,
  PRIMARY KEY("id"),
  UNIQUE("token_hash")
);`,

	`CREATE TABLE IF NOT EXISTS "status_tags"(
  "status_id" VARCHAR NOT NULL,
  "tag_id" VARCHAR NOT NULL,
  PRIMARY KEY("status_id", "tag_id")
);`,

	`CREATE TABLE IF NOT EXISTS "statuses"(
  "id" text NOT NULL,
  "uri" VARCHAR NOT NULL,
  "url" VARCHAR,
  "created_at" timestamp NOT NULL,
  "updated_at" timestamp NOT NULL,
  "account_id" VARCHAR NOT NULL,
  "content_html" VARCHAR NOT NULL,
  "content_source" VARCHAR,
  "summary" VARCHAR,
  "sensitive" BOOLEAN NOT NULL DEFAULT false,
  "visibility" VARCHAR NOT NULL DEFAULT 'public',
  "language" VARCHAR,
  "in_reply_to_id" VARCHAR,
  "in_reply_to_uri" VARCHAR,
  "boost_of_id" VARCHAR,
  "conversation_id" VARCHAR,
  "edited_at" timestamp,
  "favourites_count" INTEGER NOT NULL DEFAULT 0,
  "reblogs_count" INTEGER NOT NULL DEFAULT 0,
  "replies_count" INTEGER NOT NULL DEFAULT 0,
  pinned BOOLEAN NOT NULL DEFAULT FALSE,
  context_fetched_at TIMESTAMP NULL,
  context_only BOOLEAN NOT NULL DEFAULT FALSE,
  quote_of_id TEXT NULL,
  quote_of_uri TEXT NULL,
  quote_state TEXT NULL,
  quote_approval_uri TEXT NULL,
  content_type TEXT NOT NULL DEFAULT 'text/plain',
  "is_blog_post" BOOLEAN NOT NULL DEFAULT FALSE,
  expires_at TIMESTAMPTZ,
  "blog_slug" TEXT NOT NULL DEFAULT '',
  PRIMARY KEY("id"),
  UNIQUE("uri")
);`,

	`CREATE TABLE IF NOT EXISTS "tags"(
  "id" text NOT NULL,
  "name" VARCHAR NOT NULL,
  "url" VARCHAR NOT NULL,
  "statuses_count" INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY("id"),
  UNIQUE("name")
);`,

	`CREATE TABLE IF NOT EXISTS tombstones(
  id TEXT PRIMARY KEY,
  uri TEXT NOT NULL UNIQUE,
  former_type TEXT NOT NULL,
  deleted_at TIMESTAMP NOT NULL,
  created_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS vapid_keys(
  id TEXT PRIMARY KEY,
  public_key TEXT NOT NULL,
  private_key TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS "status_previews"(
  "status_id" text NOT NULL,
  "source_url" VARCHAR NOT NULL,
  "url" VARCHAR NOT NULL,
  "title" VARCHAR NOT NULL,
  "description" VARCHAR,
  "image" VARCHAR,
  "image_remote" VARCHAR,
  "site_name" VARCHAR,
  "fetched_at" timestamp NOT NULL,
  "cached_at" timestamp,
  PRIMARY KEY("status_id"),
  FOREIGN KEY(status_id) REFERENCES statuses(id) ON DELETE CASCADE
);`,

	`CREATE TABLE IF NOT EXISTS commercial_domain_blocks(
  domain TEXT PRIMARY KEY,
  organization TEXT,
  service TEXT,
  reference TEXT
);`,

	`CREATE TABLE IF NOT EXISTS domain_allows(
  id TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL,
  domain TEXT NOT NULL UNIQUE
);`,

	`CREATE TABLE IF NOT EXISTS featured_tags(
  id TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL,
  account_id VARCHAR NOT NULL,
  tag_id VARCHAR NOT NULL,
  UNIQUE(account_id, tag_id)
);`,

	`CREATE TABLE IF NOT EXISTS federation_policy_state(
  id INTEGER PRIMARY KEY CHECK (id = 1),
  mode TEXT NOT NULL DEFAULT 'open' CHECK (mode IN ('open', 'allowlist')),
  commercial_bigtech_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  source_url TEXT,
  etag TEXT,
  last_modified TEXT,
  last_attempt TIMESTAMP,
  last_success TIMESTAMP,
  last_error TEXT,
  lease_until TIMESTAMP
);`,

	`CREATE TABLE IF NOT EXISTS "instance_meta"(
  "id" integer PRIMARY KEY,
  "short_description" TEXT NOT NULL DEFAULT '',
  "description" TEXT NOT NULL DEFAULT '',
  "extended_description" TEXT NOT NULL DEFAULT '',
  "terms_of_service" TEXT NOT NULL DEFAULT '',
  "contact_email" TEXT NOT NULL DEFAULT '',
  "rules" TEXT NOT NULL DEFAULT '',
  "updated_at" TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS mesh_identity(
  id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  private_key TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);`,

	`CREATE TABLE IF NOT EXISTS welcome_direct_messages(
  id INTEGER PRIMARY KEY CHECK (id = 1),
  sender_account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  content TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);`,
}

var initialSchemaIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_account_deletion_destinations_pending ON account_deletion_destinations(
  run_id,
  delivered_at
);`,

	`CREATE INDEX IF NOT EXISTS idx_account_exports_account_created ON account_exports(
  account_id,
  created_at
);`,

	`CREATE INDEX IF NOT EXISTS idx_account_exports_expiry ON account_exports(state, expires_at);`,

	`CREATE INDEX IF NOT EXISTS idx_account_exports_updated_at ON account_exports(updated_at);`,

	`CREATE INDEX IF NOT EXISTS idx_account_imports_updated_at ON account_imports(updated_at);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_actor_refresh_due ON accounts(
  actor_next_refresh_at,
  id
) WHERE domain IS NOT NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_accounts_actor_uri" ON "accounts"("actor_uri");`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_avatar ON accounts(avatar);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_domain ON accounts(domain) WHERE domain IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_email ON accounts(email);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_header ON accounts(header);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_local_email_unique
ON accounts(
  LOWER(email)
) WHERE domain IS NULL
    AND email <> '';`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_local_username_unique
ON accounts(
  LOWER(username)
) WHERE domain IS NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_remote_first_seen ON accounts(first_seen_at) WHERE domain IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_silenced_present
ON accounts(moderation_state) WHERE moderation_state IN ('silenced', 'quiet');`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_suspended_present
ON accounts(suspended_at) WHERE suspended_at IS NOT NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_accounts_username_domain" ON "accounts"(
  "username",
  "domain"
);`,

	`CREATE INDEX IF NOT EXISTS "idx_acct_domain_block_account" ON "account_domain_blocks"(
  "account_id"
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_blocks_account_id_target_account_id" ON "blocks"(
  "account_id",
  "target_account_id"
);`,

	`CREATE INDEX IF NOT EXISTS idx_blocks_target_account ON blocks(
  target_account_id,
  account_id
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_bookmarks_account_id_status_id" ON "bookmarks"(
  "account_id",
  "status_id"
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_domain_blocks_domain" ON "domain_blocks"("domain");`,

	`CREATE INDEX IF NOT EXISTS idx_email_verifications_account ON email_verifications(
  account_id
);`,

	`CREATE INDEX IF NOT EXISTS idx_email_verifications_expires_at ON email_verifications(expires_at);`,

	`CREATE INDEX IF NOT EXISTS idx_email_verifications_token ON email_verifications(token_hash);`,

	`CREATE INDEX IF NOT EXISTS "idx_emoji_cached_at" ON "custom_emojis"("cached_at");`,

	`CREATE INDEX IF NOT EXISTS idx_emoji_reactions_status_key ON emoji_reactions(status_id, reaction_key, created_at);`,

	`CREATE INDEX IF NOT EXISTS idx_emojis_uncached ON custom_emojis(
  created_at
) WHERE cached_at IS NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_favourites_account_id_status_id" ON "favourites"(
  "account_id",
  "status_id"
);`,

	`CREATE INDEX IF NOT EXISTS idx_favourites_status_created ON favourites(
  status_id,
  created_at
);`,

	`CREATE INDEX IF NOT EXISTS idx_filter_keywords_filter ON filter_keywords(filter_id);`,

	`CREATE INDEX IF NOT EXISTS idx_filters_account ON filters(account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_filters_expires_at ON filters(expires_at);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_followed_tag_pair" ON "followed_tags"(
  "account_id",
  "tag_id"
);`,

	`CREATE INDEX IF NOT EXISTS "idx_followed_tag_tag" ON "followed_tags"("tag_id");`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_follows_account_id_target_account_id" ON "follows"(
  "account_id",
  "target_account_id"
);`,

	`CREATE INDEX IF NOT EXISTS "idx_follows_target_account_id" ON "follows"("target_account_id");`,

	`CREATE INDEX IF NOT EXISTS idx_home_feed_account_created ON home_feed(
  account_id,
  created_at DESC,
  status_id DESC
);`,

	`CREATE INDEX IF NOT EXISTS idx_home_feed_author ON home_feed(author_account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_home_feed_status ON home_feed(status_id);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_instances_domain" ON "instances"("domain");`,

	`CREATE INDEX IF NOT EXISTS idx_instances_updated_at ON instances(updated_at);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_invites_code" ON "invites"("code");`,

	`CREATE INDEX IF NOT EXISTS "idx_invites_created_by_id" ON "invites"("created_by_id");`,

	`CREATE INDEX IF NOT EXISTS "idx_invites_expires_at" ON "invites"("expires_at");`,

	`CREATE INDEX IF NOT EXISTS idx_job_payloads_created_at ON job_payloads(created_at);`,

	`CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(priority DESC, run_at) WHERE state = 'queued';`,

	`CREATE INDEX IF NOT EXISTS idx_list_accounts_account_id ON list_accounts(account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_list_accounts_list_id ON list_accounts(list_id);`,

	`CREATE INDEX IF NOT EXISTS idx_lists_account_id ON lists(account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_media_account ON media_attachments(account_id);`,

	`CREATE INDEX IF NOT EXISTS "idx_media_cache_failures_failed_at" ON "media_cache_failures"(
  "failed_at"
);`,

	`CREATE INDEX IF NOT EXISTS "idx_media_cached_at" ON "media_attachments"("cached_at");`,

	`CREATE INDEX IF NOT EXISTS idx_media_status_created ON media_attachments(
  status_id,
  created_at
) WHERE status_id IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_media_uncached ON media_attachments(
  created_at
) WHERE cached_at IS NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_mentions_account ON mentions(account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_mentions_status ON mentions(status_id);`,

	`CREATE INDEX IF NOT EXISTS idx_mfa_recovery_account ON mfa_recovery_codes(
  account_id,
  used_at
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_mutes_account_id_target_account_id" ON "mutes"(
  "account_id",
  "target_account_id"
);`,

	`CREATE INDEX IF NOT EXISTS idx_mutes_target_account ON mutes(target_account_id);`,

	`CREATE INDEX IF NOT EXISTS "idx_notifications_account_id_created_at" ON "notifications"(
  "account_id",
  "created_at"
);`,

	`CREATE INDEX IF NOT EXISTS idx_notifications_account_read ON notifications(account_id, read);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_api_id ON notifications(api_id);`,

	`CREATE INDEX IF NOT EXISTS idx_notifications_dedupe ON notifications(
  account_id,
  from_account_id,
  type,
  status_id,
  created_at
);`,

	`CREATE INDEX IF NOT EXISTS idx_notifications_from_account ON notifications(from_account_id);`,

	`CREATE INDEX IF NOT EXISTS idx_notifications_status ON notifications(
  status_id
) WHERE status_id IS NOT NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_status_dedup ON notifications(
  account_id,
  from_account_id,
  type,
  status_id
) WHERE status_id IS NOT NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_statusless_dedup ON notifications(
  account_id,
  from_account_id,
  type
) WHERE status_id IS NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_oauth_applications_client_id" ON "oauth_applications"(
  "client_id"
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_oauth_tokens_access_token" ON "oauth_tokens"(
  "access_token"
);`,

	`CREATE INDEX IF NOT EXISTS idx_oauth_tokens_expires_at ON oauth_tokens(expires_at);`,

	`CREATE INDEX IF NOT EXISTS idx_oauth_tokens_revoked_at ON oauth_tokens(revoked_at);`,

	`CREATE INDEX IF NOT EXISTS "idx_password_resets_account_id" ON "password_resets"(
  "account_id"
);`,

	`CREATE INDEX IF NOT EXISTS idx_password_resets_expires_at ON password_resets(expires_at);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_password_resets_token_hash" ON "password_resets"(
  "token_hash"
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_poll_options_position ON poll_options(
  poll_id,
  position
);`,

	`CREATE INDEX IF NOT EXISTS idx_poll_votes_account ON poll_votes(account_id);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_poll_votes_once ON poll_votes(
  poll_id,
  option_id,
  account_id
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_push_subscriptions_endpoint ON push_subscriptions(
  endpoint
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_push_subscriptions_oauth_token_id ON push_subscriptions(
  oauth_token_id
) WHERE oauth_token_id IS NOT NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_push_subscriptions_session_id ON push_subscriptions(
  session_id
) WHERE session_id IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS "idx_quote_auth_quoting_uri" ON "quote_authorizations"(
  "quoting_uri"
);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_relays_follow_id ON relays(
  follow_id
) WHERE follow_id IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS idx_reports_account ON reports(account_id);`,

	`CREATE INDEX IF NOT EXISTS "idx_reports_state" ON "reports"("state");`,

	`CREATE INDEX IF NOT EXISTS "idx_reports_target" ON "reports"("target_account_id");`,

	`CREATE INDEX IF NOT EXISTS idx_scheduled_due ON scheduled_statuses(scheduled_at);`,

	`CREATE INDEX IF NOT EXISTS "idx_sessions_account_id" ON "sessions"("account_id");`,

	`CREATE INDEX IF NOT EXISTS "idx_sessions_expires_at" ON "sessions"("expires_at");`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_sessions_token_hash" ON "sessions"("token_hash");`,

	`CREATE INDEX IF NOT EXISTS "idx_status_previews_cached_at" ON "status_previews"("cached_at");`,

	`CREATE INDEX IF NOT EXISTS idx_status_tags_tag_status ON status_tags(tag_id, status_id);`,

	`CREATE INDEX IF NOT EXISTS "idx_statuses_account_id_created_at" ON "statuses"(
  "account_id",
  "created_at"
);`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_blog_account_created
ON statuses(account_id, created_at DESC, id DESC)
WHERE is_blog_post = TRUE
  AND visibility = 'public'
  AND in_reply_to_id IS NULL
  AND (in_reply_to_uri IS NULL OR in_reply_to_uri = '')
  AND boost_of_id IS NULL;`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_statuses_blog_slug
ON statuses(account_id, blog_slug)
WHERE blog_slug <> '';`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_boost_created ON statuses(
  boost_of_id,
  created_at
) WHERE boost_of_id IS NOT NULL;`,

	`CREATE INDEX IF NOT EXISTS "idx_statuses_context_only" ON "statuses"("context_only");`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_created_id ON statuses(created_at, id);`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_home_head ON statuses(
  account_id,
  created_at DESC,
  id DESC
) WHERE context_only = FALSE
    AND visibility IN('public', 'unlisted', 'local', 'local_unlisted');`,

	`CREATE INDEX IF NOT EXISTS "idx_statuses_in_reply_to_id" ON "statuses"("in_reply_to_id");`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_in_reply_to_uri ON statuses(in_reply_to_uri);`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_public_head ON statuses(
  created_at DESC,
  id DESC
) WHERE context_only = FALSE
    AND visibility = 'public';`,

	`CREATE INDEX IF NOT EXISTS "idx_statuses_quote_of_id" ON "statuses"("quote_of_id");`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_quote_of_uri ON statuses(quote_of_uri);`,

	`CREATE UNIQUE INDEX IF NOT EXISTS "idx_statuses_uri" ON "statuses"("uri");`,

	`CREATE INDEX IF NOT EXISTS idx_tags_name_lower ON tags(lower(name));`,

	`CREATE INDEX IF NOT EXISTS idx_tombstones_created ON tombstones(created_at);`,

	`CREATE INDEX IF NOT EXISTS idx_tombstones_uri ON tombstones(uri);`,
}

var initialSQLiteIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_accounts_display_name_prefix ON accounts(display_name COLLATE NOCASE);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_username_prefix ON accounts(username COLLATE NOCASE);`,

	`CREATE INDEX IF NOT EXISTS idx_statuses_public_compat
ON statuses(visibility, context_only, created_at DESC);`,

	`CREATE INDEX IF NOT EXISTS idx_tags_name_prefix ON tags(name COLLATE NOCASE);`,
}

var initialPostgresIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_tags_name_prefix ON tags(lower(name) text_pattern_ops);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_username_prefix ON accounts(lower(username) text_pattern_ops);`,

	`CREATE INDEX IF NOT EXISTS idx_accounts_display_name_prefix ON accounts(lower(display_name) text_pattern_ops);`,
}
