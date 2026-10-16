# Changelog

Versions use `YY.MM.DD`, as recorded in `VERSION`.

## 26.10.13

- Alt text changed while editing a post from a Mastodon client (Mastodon web,
  Phanpy, Elk, Ivory, Tusky and others) was silently dropped. It is now saved
  and sent to other servers with the edit.
- Alt text of images already attached to a post can now be edited from the web
  composer.
- When a remote author adds or changes alt text by editing a post, the change
  now shows up here too. Before, the post kept its old description, and stayed
  hidden for anyone filtering out media without alt text.
- `PUT /api/v1/media/:id` can now clear a description, and refuses media that
  is already attached to a post, as Mastodon does. Use the status edit instead.
- Alt text is limited to 1500 characters on the server too, not only in the
  browser. Longer remote descriptions are cut at the same length. The limit is
  reported as `description_limit` in `/api/v2/instance`.
- Media without alt text now report `description` as `null` in the API, so
  clients no longer show an empty ALT badge.
- New Favourites page in the web interface, in the main menu, listing the
  posts you liked, most recent first.
- Paging through `/api/v1/favourites` skipped or repeated posts after the
  first page. It now continues from the last post shown, like bookmarks.
- When the timeline tabs or the top menu do not fit on one line, they now
  show only their icons. Before, some themes cut the last items off or split
  them over two rows.
- Keep the canonical instance actor URL public in secure and allowlist modes,
  fixing a key-fetch deadlock with other authorized-fetch servers.
- With split domains, `/api/v1/instance` now reports the handle domain as `uri`,
  and actors carry a `webfinger` property with their canonical handle.
- Hashtags can now be followed from the search in Settings > Followed hashtags,
  even before any post uses them. Hashtag pages with non-ASCII names, such as
  `#caffè`, now open and follow correctly.
- New `[web] redirect_guests_to_about` sends signed-out visitors from `/` to
  `/about`, and `[web] public_blog_directory = false` puts `/blogs` and its
  sitemap index behind the login.
- `public_local_timeline = false` now also hides the links to the local
  timeline and asks for login on `/public/local` and when scrolling, not only
  on `/`.
- Signed-out visitors could load the federated and bubble timelines through
  the scrolling endpoint. They now get the login page.
- Moved profiles now show the destination and replace Follow with a link to
  the new account. Old posts remain accessible, and cached account data is
  refreshed as soon as the move is saved.
- Move notifications now show and link to the destination, while the account
  name still opens the original profile. Moves by different users stay in
  separate notification rows when grouping is enabled.
- Cached timelines now pick up new posts when blog publishing is enabled,
  without waiting for the page cache to expire. Incoming boosts also update
  open timelines through streaming.
- Profile bios can now contain up to 5,000 characters in the web editor,
  up from 500.
- The notifications page now has a Mark all as read button. 
  Clear all still deletes them.
- Follows imported from CSV are now saved before the upload is confirmed and
  resume after a restart. Settings and the API show progress, existing follows
  and failed lookups. Follow requests and their outgoing deliveries are saved
  together, and only one import can run per account. This uses the existing
  database schema.
- CSV files over `migration.max_follows_import` are now rejected instead of
  being silently truncated. Set the limit to `0` to import the whole file.
- CSV imports now leave an internal notification when they finish or stop
  after a terminal error, with a summary and a link to that import. Retries
  do not duplicate the notice; no email or push is sent.
- Multiple Mastodon API Fixes
- Replies in the web interface now keep the original post's content warning,
  both in the timeline and on the thread page.
- Media can be added to a post by dragging files onto the composer or by
  pasting an image, as well as with the file picker. Files dropped elsewhere
  on the page go to the main composer instead of replacing the page.

## 26.10.01

Initial public source release.
