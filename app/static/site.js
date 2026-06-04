/* Wyze Bridge WebUI — IDisposable design + Flask/Python backend */
(function () {
  'use strict';

  // ── Cookies ────────────────────────────────────────────────────────────
  function setCookie(name, value, days) {
    var expires = '';
    if (days) {
      var d = new Date();
      d.setTime(d.getTime() + days * 86400000);
      expires = '; expires=' + d.toUTCString();
    }
    document.cookie = name + '=' + (value || '') + expires + '; path=/';
  }
  function getCookie(name, def) {
    var eq = name + '=', ca = document.cookie.split(';');
    for (var i = 0; i < ca.length; i++) {
      var c = ca[i].trim();
      if (c.indexOf(eq) === 0) return c.substring(eq.length);
    }
    return def !== undefined ? def : null;
  }

  // ── Click-to-copy (matches IDisposable's wireCopyButtons) ──────────────
  function wireCopyButtons(root) {
    (root || document).querySelectorAll('.copy-btn[data-url]').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        var url = btn.getAttribute('data-url');
        if (!url) return;
        var orig = btn.textContent;
        var done = function () {
          btn.textContent = btn.tagName === 'CODE' ? '✓ Copied' : 'Copied!';
          btn.classList.add('copied');
          setTimeout(function () { btn.textContent = orig; btn.classList.remove('copied'); }, 1500);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(url).then(done).catch(function () { fallbackCopy(url); done(); });
        } else { fallbackCopy(url); done(); }
      });
    });
  }
  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
  }

  // ── Snap button: refresh snapshot ────────────────────────────────────
  function wireSnapButtons(root) {
    (root || document).querySelectorAll('.snap-btn[data-cam]').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        var cam = btn.getAttribute('data-cam');
        if (!cam || btn.disabled) return;
        var orig = btn.textContent;
        btn.disabled = true;
        update_img('snapshot/' + cam + '.jpg').then(function () {
          btn.textContent = '✓';
          btn.classList.add('snapped');
        }).catch(function () {
          btn.textContent = '⚠';
        }).finally(function () {
          setTimeout(function () { btn.textContent = orig; btn.classList.remove('snapped'); btn.disabled = false; }, 1500);
        });
      });
    });
  }

  // ── Notifications ─────────────────────────────────────────────────────
  function sendNotification(title, message, type) {
    type = type || 'primary';
    if (typeof Notification !== 'undefined' && Notification.permission === 'granted' && document.visibilityState !== 'visible') {
      new Notification(title, { body: message });
      return;
    }
    var t = document.createElement('div');
    t.className = 'toast is-' + type;
    t.innerHTML = '<strong>' + title + '</strong> — ' + message;
    document.body.appendChild(t);
    setTimeout(function () { t.remove(); }, 10000);
  }

  // ── Image refresh ─────────────────────────────────────────────────────
  async function update_img(oldUrl) {
    var parts = oldUrl.split('/').pop().split('?')[0].split('.');
    var cam = parts[0], ext = parts[1];
    var newUrl = 'snapshot/' + cam + '.' + ext + '?' + Date.now();
    if (ext === 'svg') newUrl = 'img/' + cam + '.' + ext;
    var resp = await fetch(newUrl);
    var tmp = new Image();
    tmp.src = newUrl;
    await tmp.decode();
    document.querySelectorAll('[src="' + oldUrl + '"],[src="' + newUrl + '"]').forEach(function (e) { e.src = newUrl; });
    document.querySelectorAll('[poster="' + oldUrl + '"],[poster="' + newUrl + '"]').forEach(function (e) { e.setAttribute('poster', newUrl); });
    return resp;
  }

  var refresh_interval = null;
  var refresh_period = -1;

  function refresh_imgs() {
    document.querySelectorAll('.refresh_img').forEach(async function (img) {
      var url = img.getAttribute(img.nodeName === 'IMG' ? 'src' : 'poster');
      if (!url) return;
      var card = document.getElementById(img.dataset.cam);
      var isBattery = card && card.dataset.battery && card.dataset.battery.toLowerCase() === 'true';
      var isConnected = img.classList.contains('connected');
      await update_img(url, !(img.classList.contains('enabled') && (!isBattery || isConnected)));
    });
  }

  function startRefresh(period) {
    clearInterval(refresh_interval);
    refresh_period = period;
    if (refresh_period > 0) refresh_interval = setInterval(refresh_imgs, refresh_period * 1000);
  }

  async function loadPreview(img) {
    var cam = img.getAttribute('data-cam');
    var oldUrl = img.getAttribute('src') || ('snapshot/' + cam + '.jpg');
    if (!oldUrl.includes(cam)) oldUrl = 'snapshot/' + cam + '.jpg';
    try {
      await update_img(oldUrl);
      img.classList.remove('loading-preview');
    } catch (e) {
      setTimeout(function () { loadPreview(img); }, 30000);
    }
  }

  // ── Drag / sort ──────────────────────────────────────────────────────
  function swap(a, b) {
    var dummy = document.createElement('span');
    a.before(dummy); b.before(a); dummy.replaceWith(b);
  }

  function sortable(parent, selector, onUpdate) {
    var dragEl;
    function onDragOver(e) {
      e.preventDefault(); e.dataTransfer.dropEffect = 'move';
      var t = e.target.closest(selector);
      if (t && t !== dragEl) swap(dragEl, t);
    }
    function onDragEnd(e) {
      e.preventDefault();
      dragEl.classList.remove('ghost');
      parent.removeEventListener('dragover', onDragOver, false);
      parent.removeEventListener('dragend', onDragEnd, false);
      onUpdate(dragEl);
    }
    function onDragStart(e) {
      if (!e.target.matches('.drag_handle')) { e.stopPropagation(); return; }
      dragEl = e.target.closest(selector);
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('Text', dragEl.textContent);
      parent.addEventListener('dragover', onDragOver, false);
      parent.addEventListener('dragend', onDragEnd, false);
      setTimeout(function () { dragEl.classList.add('ghost'); }, 0);
    }
    parent.addEventListener('dragstart', onDragStart);
  }

  function restoreSortOrder() {
    var order = getCookie('camera_order', '');
    if (!order) return;
    if (order.includes('%2C')) { order = order.replaceAll('%2C', ','); setCookie('camera_order', order); }
    var ids = order.split(',');
    ids.forEach(function (id, i) {
      var a = document.getElementById(id);
      var cameras = [...document.querySelectorAll('.camera-card[data-cam]')];
      if (a && cameras[i]) swap(a, cameras[i]);
    });
  }

  // ── HLS player ────────────────────────────────────────────────────────
  function loadHLS(video) {
    if (!video.paused && video.hls && video.hls.media) return;
    var src = video.dataset.src || video.getAttribute('src');
    if (!src) return;
    video.controls = true;
    video.classList.remove('placeholder');
    if (typeof Hls !== 'undefined' && Hls.isSupported()) {
      if (video.hls && !video.hls.destroyed) video.hls.destroy();
      var hls = new Hls({ maxLiveSyncPlaybackRate: 1.5, liveDurationInfinity: true, maxBufferHole: 5, nudgeMaxRetry: 20, liveSyncDurationCount: 0, liveMaxLatencyDurationCount: 6 });
      video.hls = hls;
      var parsed = new URL(src, location.href);
      if (parsed.username && parsed.password) {
        hls.config.xhrSetup = function (xhr) {
          xhr.setRequestHeader('Authorization', 'Basic ' + btoa(parsed.username + ':' + parsed.password));
        };
      }
      hls.on(Hls.Events.ERROR, function (evt, data) {
        if (data.type !== Hls.ErrorTypes.NETWORK_ERROR || video.classList.contains('connected')) {
          setTimeout(function () { loadHLS(video); }, 2000);
        }
      });
      hls.on(Hls.Events.MEDIA_ATTACHED, function () {
        hls.loadSource(src);
        video.muted = true;
        video.play().catch(function () {});
      });
      hls.attachMedia(video);
    } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
      fetch(src).then(function () { video.src = src; }).catch(function () { loadHLS(video); });
    }
  }

  function loadWebRTC(video) {
    if (!video.classList.contains('placeholder')) return;
    video.classList.remove('placeholder');
    video.controls = true;
    var videoFormat = getCookie('video', 'webrtc');
    fetch('signaling/' + video.dataset.cam + '?' + videoFormat)
      .then(function (r) { return r.json(); })
      .then(function (data) { new Receiver(data); });
  }

  function autoplay(action) {
    var videos = document.querySelectorAll('video');
    if (action === 'stop') {
      videos.forEach(function (v) {
        if (!v.paused) v.classList.add('resume');
        v.pause(); v.controls = false; v.classList.add('lost');
        v.removeAttribute('src'); v.load();
      });
      return;
    }
    var doAutoplay = getCookie('autoplay');
    videos.forEach(function (v) {
      var resume = v.classList.contains('resume');
      v.controls = true;
      v.classList.remove('lost', 'resume');
      if (!resume && !doAutoplay && !v.autoplay) return;
      if (v.classList.contains('hls')) loadHLS(v);
      if (v.classList.contains('webrtc')) loadWebRTC(v);
      v.play().catch(function () {});
    });
  }

  // ── Camera controls ────────────────────────────────────────────────────
  function updateControlButtons(container, group, payload) {
    if (!container) return;
    container.querySelectorAll('[data-control-group="' + group + '"]').forEach(function (btn) {
      var sel = btn.dataset.payload === payload;
      btn.classList.toggle('selected', sel);
      btn.classList.toggle('is-selected', sel);
      btn.setAttribute('aria-pressed', sel ? 'true' : 'false');
    });
  }

  function updateControlValue(container, cmd, value) {
    var node = container && container.querySelector('[data-control-value="' + cmd + '"]');
    if (!node) return;
    if (typeof value === 'boolean') {
      node.dataset.state = value ? 'on' : 'off';
      node.textContent = value ? 'On' : 'Off';
      return;
    }
    if (typeof value === 'string') {
      var n = value.trim().toLowerCase();
      if (['on', 'off', 'auto', 'unknown'].includes(n)) {
        node.dataset.state = n;
        node.textContent = n.charAt(0).toUpperCase() + n.slice(1);
        return;
      }
    }
    delete node.dataset.state;
    node.textContent = value;
  }

  function wireControls(root) {
    root.querySelectorAll('.cam-control').forEach(function (panel) {
      var cam = panel.dataset.cam;
      panel.querySelectorAll('.ctrl-choice').forEach(function (btn) {
        btn.addEventListener('click', function () {
          btn.classList.add('is-loading');
          var payload = btn.dataset.payload;
          fetch('api/' + cam + '/' + btn.dataset.cmd + (payload ? '/' + payload : ''))
            .then(function (r) { return r.json(); })
            .then(function (data) {
              if (data.status === 'success' && btn.dataset.controlGroup && payload) updateControlButtons(panel, btn.dataset.controlGroup, payload);
              if (data.status === 'success' && ['floodlight', 'ambient_light'].includes(btn.dataset.cmd)) updateControlValue(panel, btn.dataset.cmd, payload === 'on');
              if (data.status === 'success' && btn.dataset.cmd === 'night_vision' && payload) updateControlValue(panel, 'night_vision', payload);
              sendNotification(cam, btn.dataset.cmd + ': ' + data.status, data.status === 'success' ? 'primary' : 'danger');
            })
            .catch(function (e) { sendNotification(cam, btn.dataset.cmd + ': ' + e.message, 'danger'); })
            .finally(function () { btn.classList.remove('is-loading'); });
        });
      });
      panel.querySelectorAll('input[type="range"].cam-control-range').forEach(function (input) {
        updateControlValue(panel, input.dataset.cmd, input.value);
        input.addEventListener('input', function () { updateControlValue(panel, input.dataset.cmd, input.value); });
        input.addEventListener('change', function () {
          input.disabled = true;
          fetch('api/' + input.dataset.cam + '/' + input.dataset.cmd + '/' + input.value)
            .then(function (r) { return r.json(); })
            .then(function (data) {
              if (data.status === 'success') updateControlValue(panel, input.dataset.cmd, data.value !== undefined ? data.value : input.value);
              sendNotification(input.dataset.cam, input.dataset.cmd + ': ' + data.status, data.status === 'success' ? 'primary' : 'danger');
            })
            .catch(function (e) { sendNotification(input.dataset.cam, input.dataset.cmd + ': ' + e.message, 'danger'); })
            .finally(function () { input.disabled = false; });
        });
      });
    });
  }

  // ── Battery ───────────────────────────────────────────────────────────
  function updateBatteryLevel(card) {
    if (card.dataset.battery && card.dataset.battery.toLowerCase() !== 'true') return;
    var icon = card.querySelector('.icon.battery');
    if (!icon) return;
    fetch('api/' + card.id + '/battery')
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (data.status !== 'success' || !data.value) return;
        var lvl = parseInt(data.value);
        var tier = lvl > 90 ? 'full' : lvl > 75 ? 'three-quarters' : lvl > 50 ? 'half' : lvl > 10 ? 'quarter' : 'empty';
        icon.title = 'Battery: ' + lvl + '%';
        icon.textContent = { full: '🔋', 'three-quarters': '🔋', half: '🪫', quarter: '🪫', empty: '🪫' }[tier];
        if (tier === 'empty') icon.style.color = 'var(--error)';
      });
  }

  // ── SSE ────────────────────────────────────────────────────────────────
  var sse_error_timeout = null;
  var sse_last_activity = Date.now();

  function initSSE() {
    var sse = new EventSource('api/sse_status');

    function markActive() {
      sse_last_activity = Date.now();
      if (sse_error_timeout) { clearTimeout(sse_error_timeout); sse_error_timeout = null; }
    }

    sse.addEventListener('open', function () {
      markActive();
      var el = document.getElementById('connection-lost');
      if (el) el.style.display = 'none';
      autoplay();
      startRefresh(parseInt(getCookie('refresh_period', 30)));
    });

    sse.addEventListener('ping', function () { markActive(); });

    sse.addEventListener('error', function () {
      if (sse_error_timeout) return;
      sse_error_timeout = setTimeout(function () {
        if (sse.readyState === EventSource.OPEN || Date.now() - sse_last_activity < 20000) {
          sse_error_timeout = null; return;
        }
        startRefresh(0);
        var el = document.getElementById('connection-lost');
        if (el) el.style.display = 'block';
        autoplay('stop');
        document.querySelectorAll('[data-enabled=True] .state-badge').forEach(function (b) {
          b.className = 'state-badge error'; b.textContent = 'error';
        });
        sse_error_timeout = null;
      }, 5000);
    });

    sse.addEventListener('message', function (e) {
      markActive();
      var data = JSON.parse(e.data);
      for (var cam in data) {
        var msg = data[cam];
        var card = document.getElementById(cam);
        if (!card) continue;
        var badge = card.querySelector('.state-badge');
        var preview = card.querySelector('img.refresh_img, video[data-cam="' + cam + '"]');
        var motionIcon = card.querySelector('.icon.motion');
        var connected = card.dataset.connected && card.dataset.connected.toLowerCase() === 'true';

        updateBatteryLevel(card);

        var ctrlPanel = card.querySelector('.cam-control');
        updateControlButtons(ctrlPanel, 'stream-enabled', msg.status === 'disabled' ? 'disable' : 'enable');
        updateControlButtons(ctrlPanel, 'stream-running', ['connected', 'connecting'].includes(msg.status) ? 'start' : 'stop');

        card.dataset.connected = 'false';
        if (preview) {
          preview.classList.toggle('connected', msg.status === 'connected');
          preview.classList.toggle('enabled', msg.status !== 'disabled');
        }
        if (motionIcon) {
          if (msg.motion) { motionIcon.classList.remove('is-hidden'); sendNotification('Motion', 'Motion on ' + cam, 'info'); }
          else { motionIcon.classList.add('is-hidden'); }
        }

        // Update detail overlay badge if open for this cam
        var detailBadge = document.querySelector('#detail-overlay .state-badge[data-cam="' + cam + '"]');

        switch (msg.status) {
          case 'connected':
            if (!connected) sendNotification('Connected', cam, 'primary');
            card.dataset.connected = 'true';
            if (badge) { badge.className = 'state-badge streaming'; badge.textContent = 'streaming'; }
            if (detailBadge) { detailBadge.className = 'state-badge streaming'; detailBadge.textContent = 'streaming'; }
            autoplay();
            break;
          case 'connecting':
            if (badge) { badge.className = 'state-badge connecting'; badge.textContent = 'connecting'; }
            if (detailBadge) { detailBadge.className = 'state-badge connecting'; detailBadge.textContent = 'connecting'; }
            break;
          case 'stopped':
            if (connected) sendNotification('Disconnected', cam, 'danger');
            if (badge) { badge.className = 'state-badge offline'; badge.textContent = 'stopped'; }
            if (detailBadge) { detailBadge.className = 'state-badge offline'; detailBadge.textContent = 'stopped'; }
            break;
          case 'offline':
            if (connected) sendNotification('Offline', cam, 'danger');
            if (badge) { badge.className = 'state-badge error'; badge.textContent = 'offline'; }
            if (detailBadge) { detailBadge.className = 'state-badge error'; detailBadge.textContent = 'offline'; }
            break;
          default:
            if (connected) sendNotification('Disconnected', cam, 'danger');
            if (badge) { badge.className = 'state-badge error'; badge.textContent = 'error'; }
            if (detailBadge) { detailBadge.className = 'state-badge error'; detailBadge.textContent = 'error'; }
        }
      }
    });
  }

  // ── Camera Detail Overlay (matches IDisposable's cameraHTML) ───────────
  var detailOverlay = null;
  var detailVideo = null;

  function buildControlsHTML(data) {
    var controls = (data.camera_info && data.camera_info.controls) ? data.camera_info.controls : {};
    var cam = data.name_uri || '';
    var html = '<div class="detail-controls"><h3>Controls</h3>';

    // Power
    html += '<div class="ctrl-section">';
    html += '<div class="ctrl-section-label">Power</div>';
    html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '">';
    html += '<button class="ctrl-choice" data-cmd="power" data-payload="on">On</button>';
    html += '<button class="ctrl-choice" data-cmd="power" data-payload="off">Off</button>';
    html += '<button class="ctrl-choice" data-cmd="power" data-payload="restart">Restart</button>';
    html += '</div></div>';

    // Stream
    var enabled = data.enabled;
    var connected = data.connected;
    html += '<div class="ctrl-section">';
    html += '<div class="ctrl-section-label">Stream</div>';
    html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '">';
    html += '<button class="ctrl-choice' + (enabled ? ' selected' : '') + '" data-control-group="stream-enabled" data-cmd="state" data-payload="enable">Enable</button>';
    html += '<button class="ctrl-choice' + (!enabled ? ' selected' : '') + '" data-control-group="stream-enabled" data-cmd="state" data-payload="disable">Disable</button>';
    html += '</div>';
    html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '">';
    html += '<button class="ctrl-choice' + (connected ? ' selected' : '') + '" data-control-group="stream-running" data-cmd="state" data-payload="start">Start</button>';
    html += '<button class="ctrl-choice' + (!connected ? ' selected' : '') + '" data-control-group="stream-running" data-cmd="state" data-payload="stop">Stop</button>';
    html += '</div></div>';

    if (data.product_model === 'LD_CFP') {
      var fl = controls.floodlight_on;
      var al = controls.ambient_light_on;
      var ab = controls.ambient_brightness || 0;
      html += '<div class="ctrl-section"><div class="ctrl-section-label">Floodlight <span class="ctrl-tag" data-control-value="floodlight" data-state="' + (fl === true ? 'on' : fl === false ? 'off' : 'unknown') + '">' + (fl === true ? 'On' : fl === false ? 'Off' : '?') + '</span></div>';
      html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '"><button class="ctrl-choice' + (fl === true ? ' selected' : '') + '" data-control-group="floodlight" data-cmd="floodlight" data-payload="on">On</button><button class="ctrl-choice' + (fl === false ? ' selected' : '') + '" data-control-group="floodlight" data-cmd="floodlight" data-payload="off">Off</button></div></div>';

      html += '<div class="ctrl-section"><div class="ctrl-section-label">Ambient Light <span class="ctrl-tag" data-control-value="ambient_light" data-state="' + (al === true ? 'on' : al === false ? 'off' : 'unknown') + '">' + (al === true ? 'On' : al === false ? 'Off' : '?') + '</span></div>';
      html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '"><button class="ctrl-choice' + (al === true ? ' selected' : '') + '" data-control-group="ambient-light" data-cmd="ambient_light" data-payload="on">On</button><button class="ctrl-choice' + (al === false ? ' selected' : '') + '" data-control-group="ambient-light" data-cmd="ambient_light" data-payload="off">Off</button></div>';
      html += '<div class="slider-label"><span>Brightness</span><span class="ctrl-tag" data-control-value="ambient_brightness">' + ab + '</span></div>';
      html += '<input type="range" class="cam-control-range" data-cam="' + cam + '" data-cmd="ambient_brightness" min="0" max="100" value="' + ab + '"></div>';
    } else {
      var nv = controls.night_vision || 'unknown';
      html += '<div class="ctrl-section"><div class="ctrl-section-label">Night Vision <span class="ctrl-tag" data-control-value="night_vision" data-state="' + nv + '">' + (nv.charAt(0).toUpperCase() + nv.slice(1)) + '</span></div>';
      html += '<div class="ctrl-btn-row cam-control" data-cam="' + cam + '">';
      html += '<button class="ctrl-choice' + (nv === 'on' ? ' selected' : '') + '" data-control-group="night-vision" data-cmd="night_vision" data-payload="on">On</button>';
      html += '<button class="ctrl-choice' + (nv === 'off' ? ' selected' : '') + '" data-control-group="night-vision" data-cmd="night_vision" data-payload="off">Off</button>';
      html += '<button class="ctrl-choice' + (nv === 'auto' ? ' selected' : '') + '" data-control-group="night-vision" data-cmd="night_vision" data-payload="auto">Auto</button>';
      html += '</div></div>';
    }

    html += '</div>';
    return html;
  }

  function buildDetailRows(data) {
    var info = data.camera_info || {};
    var basic = info.basicInfo || {};
    var rows = [
      ['Model', (data.model_name || data.product_model || '') + (data.product_model ? ' (' + data.product_model + ')' : '')],
      ['State', '<span class="state-badge ' + (data.connected ? 'streaming' : 'offline') + '" data-cam="' + (data.name_uri || '') + '">' + (data.connected ? 'streaming' : 'offline') + '</span>'],
      ['IP', basic.ip || data.ip || ''],
      ['MAC', basic.mac || data.mac || ''],
      ['Firmware', basic.firmware || data.firmware_ver || ''],
    ];
    if (data.is_battery) rows.push(['Battery', 'Yes']);
    if (data.substream) rows.push(['Sub-stream', 'Yes']);
    if (info.sdParm && info.sdParm.status === '1') rows.push(['SD Card', Math.round(info.sdParm.free / 1024) + ' GB free']);
    return rows.filter(function (r) { return r[1]; }).map(function (r) {
      return '<tr><td>' + r[0] + '</td><td>' + r[1] + '</td></tr>';
    }).join('');
  }

  function openDetail(camUri) {
    if (!detailOverlay) return;
    var card = document.getElementById(camUri);
    var nickname = card ? (card.querySelector('.camera-info h3') || {}).textContent || camUri : camUri;

    // Header
    detailOverlay.querySelector('h1').innerHTML = escapeHtml(nickname) + ' <span class="version"></span>';
    detailOverlay.querySelector('.version').textContent = '';

    // Clear and show placeholder
    var playerCont = detailOverlay.querySelector('.player-container');
    playerCont.innerHTML = '';
    var metaTable = detailOverlay.querySelector('.camera-meta tbody');
    metaTable.innerHTML = '<tr><td colspan="2" style="color:var(--text-dim)">Loading…</td></tr>';
    detailOverlay.querySelector('.stream-urls .url-list').innerHTML = '';
    detailOverlay.querySelector('.actions').innerHTML = '';
    var ctrlsEl = detailOverlay.querySelector('.detail-controls');
    if (ctrlsEl) ctrlsEl.remove();

    detailOverlay.classList.add('active');
    document.body.style.overflow = 'hidden';

    // Fetch full camera data
    fetch('api/' + camUri)
      .then(function (r) { return r.json(); })
      .then(function (data) {
        // Rows
        metaTable.innerHTML = buildDetailRows(data);

        // Stream URLs
        var urlList = detailOverlay.querySelector('.stream-urls .url-list');
        if (data.rtsp_url) urlList.innerHTML += '<code class="copy-btn" data-url="' + escapeHtml(data.rtsp_url) + '">' + escapeHtml(data.rtsp_url) + '</code>';
        if (data.hls_url) urlList.innerHTML += '<code class="copy-btn" data-url="' + escapeHtml(data.hls_url + 'index.m3u8') + '">' + escapeHtml(data.hls_url + 'index.m3u8') + '</code>';
        if (data.webrtc_url) urlList.innerHTML += '<code class="copy-btn" data-url="' + escapeHtml(data.webrtc_url) + '">' + escapeHtml(data.webrtc_url) + '</code>';
        wireCopyButtons(detailOverlay);

        // Actions
        var acts = detailOverlay.querySelector('.actions');
        acts.innerHTML =
          '<button onclick="fetch(\'api/' + camUri + '/state/start\').then(r=>r.json()).then(d=>sendNotificationGlobal(\'' + camUri + '\',\'start: \'+d.status,d.status===\'success\'?\'primary\':\'danger\'))">Start Stream</button>' +
          '<button onclick="fetch(\'api/' + camUri + '/state/stop\').then(r=>r.json()).then(d=>sendNotificationGlobal(\'' + camUri + '\',\'stop: \'+d.status,d.status===\'success\'?\'primary\':\'danger\'))">Stop Stream</button>' +
          '<button class="snap-btn" data-cam="' + camUri + '">&#128247; Snapshot</button>';
        wireSnapButtons(acts);

        // Controls
        var ctrlsHTML = buildControlsHTML(data);
        detailOverlay.querySelector('.camera-meta').insertAdjacentHTML('beforeend', ctrlsHTML);
        wireControls(detailOverlay.querySelector('.camera-meta'));

        // Player — load HLS
        if (data.hls_url) {
          var vid = document.createElement('video');
          vid.muted = true;
          vid.controls = true;
          vid.playsinline = true;
          vid.dataset.src = data.hls_url + 'index.m3u8';
          vid.dataset.cam = camUri;
          vid.className = 'hls';
          playerCont.appendChild(vid);
          detailVideo = vid;
          loadHLSLazy(vid);
        } else {
          playerCont.innerHTML = '<img src="snapshot/' + camUri + '.jpg" style="width:100%;height:100%;object-fit:contain;">';
        }
      })
      .catch(function (e) {
        metaTable.innerHTML = '<tr><td colspan="2" style="color:var(--error)">Failed to load camera data</td></tr>';
      });
  }

  function closeDetail() {
    if (!detailOverlay) return;
    detailOverlay.classList.remove('active');
    document.body.style.overflow = '';
    if (detailVideo) {
      if (detailVideo.hls) { detailVideo.hls.destroy(); }
      detailVideo.pause();
      detailVideo.src = '';
      detailVideo = null;
    }
  }

  function loadHLSLazy(video) {
    if (typeof Hls !== 'undefined') {
      loadHLS(video);
      return;
    }
    // Dynamically load HLS.js if not already present
    var s = document.createElement('script');
    s.src = 'https://cdn.jsdelivr.net/npm/hls.js/dist/hls.min.js';
    s.onload = function () { loadHLS(video); };
    document.head.appendChild(s);
  }

  function escapeHtml(str) {
    return String(str).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  // exposed for inline onclick
  window.sendNotificationGlobal = sendNotification;

  // ── Hash routing ──────────────────────────────────────────────────────
  function handleHash() {
    var hash = location.hash.replace('#', '');
    if (hash.startsWith('camera/')) {
      var cam = hash.slice(7);
      if (cam) { openDetail(cam); return; }
    }
    closeDetail();
  }

  // ── Image age display ─────────────────────────────────────────────────
  function imgAge() {
    document.querySelectorAll('span.age[data-age]').forEach(function (span) {
      var ts = parseInt(span.dataset.age);
      if (!ts) return;
      var s = Math.floor((Date.now() - ts) / 1000);
      span.textContent = s < 60 ? s + 's' : s < 3600 ? '+' + Math.floor(s / 60) + 'm' : '+' + Math.floor(s / 3600) + 'h';
    });
    setTimeout(imgAge, 1000);
  }

  // ── Init ──────────────────────────────────────────────────────────────
  document.addEventListener('DOMContentLoaded', function () {
    // Build detail overlay element (matches IDisposable's cameraHTML structure)
    detailOverlay = document.createElement('div');
    detailOverlay.id = 'detail-overlay';
    detailOverlay.innerHTML = [
      '<header>',
        '<a href="#" class="back" id="detail-back">&#8592; All Cameras</a>',
        '<h1></h1>',
      '</header>',
      '<div class="camera-detail">',
        '<div class="player-container"></div>',
        '<div class="camera-meta">',
          '<table><tbody></tbody></table>',
          '<div class="stream-urls">',
            '<h3>Stream URLs <small>(click to copy)</small></h3>',
            '<div class="url-list"></div>',
          '</div>',
          '<div class="actions"></div>',
        '</div>',
      '</div>',
    ].join('');
    document.body.appendChild(detailOverlay);

    detailOverlay.querySelector('#detail-back').addEventListener('click', function (e) {
      e.preventDefault();
      history.pushState('', document.title, location.pathname + location.search);
      closeDetail();
    });

    // Hash routing
    window.addEventListener('hashchange', handleHash);
    if (location.hash.startsWith('#camera/')) handleHash();

    // Copy buttons on the grid
    wireCopyButtons();

    // Snap buttons on the grid
    wireSnapButtons();

    // Drag / sort
    var grid = document.querySelector('.camera-grid');
    if (grid) {
      restoreSortOrder();
      sortable(grid, '.camera-card[data-cam]', function () {
        var cameras = document.querySelectorAll('.camera-card[data-cam]');
        var order = [...cameras].map(function (c) { return c.id; }).filter(Boolean).join(',');
        setCookie('camera_order', order);
      });
    }

    // Snapshot refresh on load
    document.querySelectorAll('.loading-preview').forEach(loadPreview);

    // Image age
    imgAge();

    // HLS placeholder — click to load
    document.querySelectorAll('video.hls.placeholder').forEach(function (video) {
      video.parentElement.addEventListener('click', function () {
        loadHLS(video);
        video.play().catch(function () {});
      }, { once: true });
      video.addEventListener('play', function () {
        loadHLS(video);
        if (!video.classList.contains('connected') && !video.hasAttribute('connecting')) {
          video.setAttribute('connecting', '');
          fetch('api/' + video.dataset.cam + '/start').then(function () {
            video.classList.add('connected');
            video.removeAttribute('connecting');
          }).catch(function () { video.removeAttribute('connecting'); });
        }
        video.setAttribute('autoplay', '');
      });
      video.addEventListener('pause', function () { video.removeAttribute('autoplay'); });
    });

    // WebRTC
    document.querySelectorAll('[data-enabled=True] video.webrtc.placeholder').forEach(function (video) {
      video.parentElement.addEventListener('click', function () { video.play(); }, { once: true });
      video.addEventListener('play', function () { loadWebRTC(video); }, { once: true });
      video.addEventListener('pause', function () { video.removeAttribute('autoplay'); });
    });

    // SSE
    initSSE();
    startRefresh(parseInt(getCookie('refresh_period', 30)));

    // Notification permission
    if (typeof Notification !== 'undefined' && Notification.permission !== 'granted') {
      Notification.requestPermission().catch(function () {});
    }
  });

})();
