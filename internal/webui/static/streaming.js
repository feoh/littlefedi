(function () {
  'use strict';

  var retry = 1000;
  var socket;
  var currentStream = null;
  var currentTag = '';
  var currentList = '';
  var notificationPoll = null;
  var notificationFallback = null;
  var reconnectTimer = null;
  var homeRefreshKey = 'littlefedi.refresh.home.v1';
  var notificationRefreshKey = 'littlefedi.refresh.notifications.v1';
  // Persistent connection state so components rebuilt by hx-boost navigation
  // (which miss the one-shot stream-open/-close events) can read the current
  // status instead of assuming the stream is down.
  var state = window.littlefediStream = window.littlefediStream || {open: false};

  function autoRefreshEnabled(key) {
    try { return localStorage.getItem(key) !== '0'; } catch (_) { return true; }
  }

  function bindRefreshSettings() {
    [
      ['auto-refresh-home', homeRefreshKey],
      ['auto-refresh-notifications', notificationRefreshKey]
    ].forEach(function (setting) {
      var toggle = document.getElementById(setting[0]);
      if (!toggle || toggle.dataset.bound) return;
      toggle.dataset.bound = 'true';
      toggle.checked = autoRefreshEnabled(setting[1]);
      toggle.addEventListener('change', function () {
        try { localStorage.setItem(setting[1], toggle.checked ? '1' : '0'); } catch (_) {}
      });
    });
  }

  // Streamed items are inserted above whatever the reader is on, which is
  // silent and invisible to anyone not watching the top of the page. Say so
  // through the polite region base.html keeps for the rest of the app.
  function announce(name, fallback) {
    var region = document.getElementById('hx-loading-status');
    if (!region) return;
    var strings = document.getElementById('js-strings');
    var text = (strings && strings.dataset[name]) || fallback;
    region.textContent = '';
    window.setTimeout(function () { region.textContent = text; }, 50);
  }

  function streamName() {
    var timeline = document.querySelector('#timeline[data-stream]');
    return timeline && timeline.dataset.stream;
  }

  function streamTag() {
    var timeline = document.querySelector('#timeline[data-stream]');
    return (timeline && timeline.dataset.streamTag) || '';
  }

  function streamList() {
    var timeline = document.querySelector('#timeline[data-stream]');
    return (timeline && timeline.dataset.streamList) || '';
  }

  function statusID(payload) {
    try { return JSON.parse(payload).id || ''; } catch (_) { return ''; }
  }

  // Mirror the server-side hide-replies/hide-boosts timeline filtering so a
  // streamed status doesn't slip in when the SSR page (and a reload) would
  // exclude it. Only applies to brand-new inserts; edits to already-visible
  // statuses (status.update) are refreshed regardless.
  function streamFiltered(payload) {
    var status;
    try { status = JSON.parse(payload); } catch (_) { return false; }
    if (document.body.dataset.hideBoosts && status.reblog) return true;
    if (document.body.dataset.hideReplies && status.in_reply_to_id) return true;
    return false;
  }

  function setNotificationCount(count) {
    count = parseInt(count, 10) || 0;
	document.querySelectorAll('[data-notification-badge]').forEach(function (badge) {
	  badge.textContent = count > 99 ? '99+' : String(count);
	  badge.hidden = count < 1;
	});
  }

  function refreshNotificationCount() {
    if (!document.body.dataset.authenticated) return;
    fetch('/web/notification-count', {credentials: 'same-origin'})
      .then(function (r) { if (!r.ok) throw new Error(); return r.text(); })
      .then(setNotificationCount)
      .catch(function () {});
  }

  var markingNotificationsRead = false;

  function bindNotificationReadShortcut() {
    if (!window.matchMedia('(display-mode: standalone)').matches && !navigator.standalone) return;
    document.querySelectorAll('a[data-notifications-read]').forEach(function (link) {
      if (link.dataset.readBound) return;
      link.dataset.readBound = 'true';
      link.classList.add('notification-read-shortcut');
      link.title = link.dataset.readHint;
      var timer;
      var held = false;
      var startX;
      var startY;

      function cancel() {
        clearTimeout(timer);
      }

      link.addEventListener('pointerdown', function (event) {
        cancel();
        held = false;
        if (event.button !== 0 || !event.isPrimary) return;
        startX = event.clientX;
        startY = event.clientY;
        timer = setTimeout(function () {
          held = true;
          if (markingNotificationsRead) return;
          markingNotificationsRead = true;
          var unread = document.querySelectorAll('.notification.unread');
          fetch('/notifications/read', {
            method: 'POST', credentials: 'same-origin',
            headers: {'Accept': 'application/json', 'X-CSRF-Token': link.dataset.notificationsRead}
          })
            .then(function (response) {
              if (response.status !== 204) throw new Error();
              unread.forEach(function (row) { row.classList.remove('unread'); });
              setNotificationCount(0);
              refreshNotificationCount();
              announce('notificationsRead', 'All notifications marked as read.');
            })
            .catch(function () {
              var strings = document.getElementById('js-strings');
              window.alert((strings && strings.dataset.requestFailed) || 'Request failed.');
            })
            .finally(function () { markingNotificationsRead = false; });
        }, 600);
      });
      link.addEventListener('pointermove', function (event) {
        if (Math.abs(event.clientX - startX) > 10 || Math.abs(event.clientY - startY) > 10) cancel();
      });
      ['pointerup', 'pointercancel', 'pointerleave'].forEach(function (type) {
        link.addEventListener(type, cancel);
      });
      link.addEventListener('contextmenu', function (event) { event.preventDefault(); });
      // Consume the click after a hold before hx-boost navigates away.
      link.addEventListener('click', function (event) {
        if (!held || event.detail === 0) return;
        held = false;
        event.preventDefault();
        event.stopImmediatePropagation();
      }, true);
    });
  }

  function stopNotificationPolling() {
    if (notificationFallback) {
      clearTimeout(notificationFallback);
      notificationFallback = null;
    }
    if (notificationPoll) {
      clearInterval(notificationPoll);
      notificationPoll = null;
    }
  }

  function startNotificationPolling() {
    if (!document.body.dataset.authenticated || notificationPoll) return;
    refreshNotificationCount();
    // A hidden tab has no badge to look at, so skip the round trip and catch
    // up on visibilitychange instead.
    notificationPoll = setInterval(function () {
      if (!document.hidden) refreshNotificationCount();
    }, 30000);
  }

  function armNotificationFallback() {
    if (!document.body.dataset.authenticated || state.open || notificationFallback || notificationPoll) return;
    notificationFallback = setTimeout(function () {
      notificationFallback = null;
      if (!state.open) startNotificationPolling();
    }, 5000);
  }

  function refreshStatus(id, replace) {
    if (!id) return;
    var url = '/partial/status/' + encodeURIComponent(id);
    var stream = document.querySelector('#timeline[data-stream]');
    var scope = stream && /^(public|hashtag)/.test(stream.dataset.stream) ? 'public' : 'home';
    // New timeline entries are filtered again by the server. The stream JSON
    // can be incomplete for remote replies and boosts, while this endpoint has
    // the stored status and the viewer's current preferences.
    url += '?scope=' + scope + (replace ? '' : '&timeline=1');
    fetch(url, {credentials: 'same-origin'})
      .then(function (r) { if (!r.ok) throw new Error(); return r.text(); })
      .then(function (html) {
        if (!html.trim()) return;
        var timeline = document.querySelector('#timeline[data-stream]');
        if (!timeline) return;
        var existing = document.getElementById('status-' + id);
        if (existing) existing.outerHTML = html;
        else if (!replace) {
          var empty = timeline.querySelector('.empty-state');
          if (empty) empty.remove();
          timeline.insertAdjacentHTML('afterbegin', html);
          announce('newPosts', 'New posts arrived at the top of the timeline.');
        }
        if (window.htmx) window.htmx.process(timeline);
        document.dispatchEvent(new CustomEvent('littlefedi:timeline-update'));
      }).catch(function () {});
  }

  // The notifications page's #notifications list caches the newest
  // notification ID it has rendered (data-since-id), lazily seeded from the
  // first row already on the page, so a live 'notification' stream event can
  // fetch and prepend just what's new instead of requiring a reload.
  function prependNewNotifications() {
    var container = document.getElementById('notifications');
    if (!container) return;
    var sinceID = container.dataset.sinceId;
    if (!sinceID) {
      var first = container.querySelector('.notification[data-id]');
      if (!first) return; // empty state - nothing to diff against yet
      sinceID = first.dataset.id;
    }
    var params = new URLSearchParams({since_id: sinceID});
    var filter = new URLSearchParams(location.search).get('filter');
    if (filter) params.set('filter', filter);
    fetch('/partial/notifications/new?' + params.toString(), {credentials: 'same-origin'})
      .then(function (r) { if (!r.ok) throw new Error(); return r.text(); })
      .then(function (html) {
        if (!html.trim()) { container.dataset.sinceId = sinceID; return; }
        var empty = container.querySelector('.empty-state');
        if (empty) empty.remove();
        container.insertAdjacentHTML('afterbegin', html);
        announce('newNotifications', 'New notifications arrived at the top of the list.');
        if (window.htmx) window.htmx.process(container);
        var newest = container.querySelector('.notification[data-id]');
        container.dataset.sinceId = newest ? newest.dataset.id : sinceID;
        document.dispatchEvent(new CustomEvent('littlefedi:timeline-update'));
        // The fetch above marks the new notifications read server-side;
        // re-sync the header badge to reflect that.
        refreshNotificationCount();
      }).catch(function () {});
  }

  function onMessage(message) {
    if (message.event === 'delete') {
      var deleted = document.getElementById('status-' + message.payload);
      if (deleted) deleted.remove();
      return;
    }
    if (message.event === 'notification') {
      refreshNotificationCount();
      if (autoRefreshEnabled(notificationRefreshKey)) prependNewNotifications();
      document.dispatchEvent(new CustomEvent('littlefedi:notification'));
      return;
    }
    var timeline = document.querySelector('#timeline[data-stream]');
    if (!timeline) return;
    var expected = timeline.dataset.stream;
    var streams = message.stream || [];
    if (streams.indexOf(expected) === -1) return;
    if (expected === 'user' && !autoRefreshEnabled(homeRefreshKey)) return;
    if (message.event === 'update' || message.event === 'status.update') {
      var replace = message.event === 'status.update';
      // For new insertions, honour the viewer's hide-replies/hide-boosts prefs
      // so streaming matches what a page reload would render.
      if (!replace && streamFiltered(message.payload)) return;
      refreshStatus(statusID(message.payload), replace);
    }
  }

  function connect() {
    var stream = streamName();
    // An authenticated user stream also drives the notification badge. Public
    // pages use their exact stream, while unsupported timelines keep SSR only.
    if (!stream && !document.body.dataset.authenticated) return;
    stream = stream || 'user';
    var tag = streamTag();
    var list = streamList();

    // Already connected to the same stream+tag+list - nothing to do. This makes
    // connect() safe to call on every htmx:afterSettle (partial swaps like
    // infinite scroll don't change the stream).
    if (socket && currentStream === stream && currentTag === tag && currentList === list &&
        (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) {
      return;
    }

    // Close any existing socket. Its onclose handler checks whether it is
    // still the active socket before scheduling a reconnect, so closing
    // here to switch streams won't trigger a spurious reconnect loop.
    if (socket && socket.readyState < WebSocket.CLOSING) socket.close();

    currentStream = stream;
    currentTag = tag;
    currentList = list;

    var scheme = location.protocol === 'https:' ? 'wss://' : 'ws://';
    var params = new URLSearchParams({stream: stream});
    if (tag) params.set('tag', tag);
    if (list) params.set('list', list);
    var s = new WebSocket(scheme + location.host + '/api/v1/streaming?' + params.toString());
    socket = s;
    s.onopen = function () {
      retry = 1000;
      state.open = true;
      stopNotificationPolling();
      if (document.body.dataset.authenticated && stream !== 'user') {
        s.send(JSON.stringify({type: 'subscribe', stream: 'user'}));
      }
      document.dispatchEvent(new CustomEvent('littlefedi:stream-open'));
    };
    s.onmessage = function (event) {
      try { onMessage(JSON.parse(event.data)); } catch (_) {}
    };
    s.onclose = function () {
      // If this socket was replaced by a new connection (stream switch),
      // don't dispatch stream-close or schedule a reconnect - the new
      // socket's own handlers manage state from here.
      if (s !== socket) return;
      state.open = false;
      armNotificationFallback();
      document.dispatchEvent(new CustomEvent('littlefedi:stream-close'));
      scheduleReconnect(retry);
      retry = Math.min(retry * 2, 30000);
    };
  }

  function scheduleReconnect(delay) {
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(connect, delay);
  }

  // A phone suspends the tab, the socket dies, and onclose either arrives late
  // or has already backed off to half a minute. Either way the reader is looking
  // at a stale timeline and a stale badge the moment they come back. Reconnect
  // on the events that mean "you are here again" and start the backoff over.
  function reconnectNow() {
    if (socket && socket.readyState === WebSocket.OPEN) return;
    retry = 1000;
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
    connect();
  }
  window.addEventListener('online', reconnectNow);
  window.addEventListener('pageshow', reconnectNow);

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) return;
    if (notificationPoll) refreshNotificationCount();
    reconnectNow();
  });

  document.addEventListener('DOMContentLoaded', function () {
    bindRefreshSettings();
    bindNotificationReadShortcut();
    connect();
    armNotificationFallback();
  });
  // Re-evaluate the stream after hx-boost navigations: the #timeline element
  // may have a different data-stream (e.g. user -> public) or a different
  // data-stream-tag (e.g. #golang -> #rust). connect() is a no-op when the
  // stream hasn't changed, so partial swaps (infinite scroll, status refresh)
  // are cheap.
  document.addEventListener('htmx:afterSettle', function () {
    bindRefreshSettings();
    bindNotificationReadShortcut();
    connect();
  });
  // Back/forward navigation restores a different page from htmx history;
  // reconnect immediately instead of waiting for the onclose retry delay.
  document.addEventListener('htmx:historyRestore', connect);
}());
