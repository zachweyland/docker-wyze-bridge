function setCookie(name, value, days) {
  var expires = "";
  if (days) {
    var date = new Date();
    date.setTime(date.getTime() + days * 24 * 60 * 60 * 1000);
    expires = "; expires=" + date.toUTCString();
  }
  document.cookie = name + "=" + (value || "") + expires + "; path=/";
}

function getCookie(name, def = null) {
  var nameEQ = name + "=";
  var ca = document.cookie.split(";");
  for (var i = 0; i < ca.length; i++) {
    var c = ca[i];
    while (c.charAt(0) == " ") c = c.substring(1, c.length);
    if (c.indexOf(nameEQ) == 0) return c.substring(nameEQ.length, c.length);
  }
  return def;
}

let refresh_interval = null; // refresh images interval
let refresh_period = -1; // refresh images time period in seconds
let sse_error_timeout = null;
let sse_last_activity = Date.now();

document.addEventListener("DOMContentLoaded", applyPreferences);

// Event listener for input and validate changes before applyPreferences
document.addEventListener("DOMContentLoaded", () => {
  function changeSetting(select) {
    let changeValue = Number(select.value);
    let cookieId = select.id.replace("select_", "");
    if (changeValue < select.min || changeValue > select.max) {
      select.classList.add("is-danger");
      select.value = getCookie(cookieId, cookieId == "refresh_period" ? 30 : 2);
      setTimeout(() => {
        select.classList.remove("is-danger");
      }, 1000);
      return;
    }
    setCookie(cookieId, changeValue);
    applyPreferences();
    select.classList.add("is-success");
    setTimeout(() => {
      select.classList.remove("is-success");
    }, 1000);
  }
  document
    .querySelectorAll("#select_refresh_period, #select_number_of_columns")
    .forEach((input) => {
      input.addEventListener("change", (e) => changeSetting(e.target));
    });
});

function applyPreferences() {
  const repeatNumber = getCookie("number_of_columns", 2);
  const grid = document.querySelectorAll(".camera");
  for (var i = 0, len = grid.length; i < len; i++) {
    grid[i].classList.forEach((item) => {
      if (item.match(/^is\-\d/) || item == "is-one-fifth") {
        grid[i].classList.remove(item);
      }
    });
    grid[i].classList.add(
      `is-${repeatNumber == 5 ? "one-fifth" : 12 / repeatNumber}`
    );
  }
  var sortOrder = getCookie("camera_order", "");
  if (sortOrder) {
    // clean escaped camera_order from flask args
    if (sortOrder.includes("%2C")) {
      sortOrder = sortOrder.replaceAll("%2C", ",")
      setCookie("camera_order", sortOrder);
    }
    const ids = sortOrder.split(",");
    var cameras = [...document.querySelectorAll(".camera")];
    for (var i = 0; i < Math.min(ids.length, cameras.length); i++) {
      var a = document.getElementById(ids[i]);
      var b = cameras[i];
      if (a && b)
        // only swap if they both exist
        swap(a, b);
      cameras = [...document.querySelectorAll(".camera")];
    }
  }

  const new_period = getCookie("refresh_period", 30);
  if (refresh_period != new_period) {
    refresh_period = new_period;
    console.debug("applyPreferences refresh_period", refresh_period);
    clearInterval(refresh_interval);
    if (refresh_period > 0) {
      refresh_interval = setInterval(refresh_imgs, refresh_period * 1000);
    }
  }
}

/**
 * Swap two Element
 * @param a {Element} the first element
 * @param b {Element} the second element
 */
function swap(a, b) {
  let dummy = document.createElement("span");
  a.before(dummy);
  b.before(a);
  dummy.replaceWith(b);
}

/**
 * Enable dragging/sorting under the given parent.
 * @param parent {Element} parent to sort under
 * @param selector {string} selector string, to select the elements allowed to be sorted/swapped
 * @param onUpdate {Function} fired when an element is updated
 */
function sortable(parent, selector, onUpdate = null) {
  /** The element currently being dragged */
  var dragEl;

  /**
   * Fired when dragging over another element.
   * Swap the two elements, based on their parent selector.
   * @param e {DragEvent}
   * @private
   */
  function _onDragOver(e) {
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";

    const target = e.target.closest(selector);
    if (target && target !== dragEl) {
      console.debug("_onDragOver()", target);
      swap(dragEl, target);
    }
  }

  /**
   * Fired when drag ends (release mouse).
   * Turn off the ghost css.
   * @param e {DragEvent}
   * @private
   */
  function _onDragEnd(e) {
    console.debug("_onDragEnd()", e.target);
    e.preventDefault();
    dragEl.classList.remove("ghost");
    parent.removeEventListener("dragover", _onDragOver, false);
    parent.removeEventListener("dragend", _onDragEnd, false);
    onUpdate(dragEl);
  }

  /**
   * Fired when starting a drag.
   * Only allow dragging of elements that match our selector
   * @param e {DragEvent}
   * @private
   */
  function _onDragStart(e) {
    if (!e.target.matches(".drag_handle")) {
      e.stopPropagation();
      return;
    }
    dragEl = e.target.closest(selector);
    console.debug("_onDragStart()", e.target);
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("Text", dragEl.textContent);

    parent.addEventListener("dragover", _onDragOver, false);
    parent.addEventListener("dragend", _onDragEnd, false);

    setTimeout(function () {
      dragEl.classList.add("ghost");
    }, 0);
  }

  parent.addEventListener("dragstart", _onDragStart);
}

/**
 * Update camera image.
 * Self contained to fetch the new url, pre-decode it, and update the img.src, video.poster and videojs div overlay.
 * @param oldUrl the img url, could either be img/cam-name.jpg or snapshot/cam-name.jpg
 * @returns {Promise<void>}
 */
async function update_img(oldUrl, useImg = false) {
  let [cam, ext] = oldUrl.split("/").pop().split("?")[0].split(".");
  let newUrl = "snapshot/" + cam + "." + ext + "?" + Date.now();
  if (useImg || ext == "svg") {
    newUrl = "img/" + cam + "." + ext;
  }
  let button = document.querySelector(`.update-preview[data-cam="${cam}"]`);
  if (button) {
    button.disabled = true;
    button.getElementsByClassName("fas")[0].classList.add("fa-spin");
    button.style.display = "inline-block";
  }

  let imgDate = await fetch(newUrl);
  // reduce img flicker by pre-decode, before swapping it
  const tmp = new Image();
  tmp.src = newUrl;
  await tmp.decode();

  // update img.src
  document
    .querySelectorAll(`[src="${oldUrl}"],[src="${newUrl}"]`)
    .forEach(function (e) {
      e.src = newUrl;
    });

  // update video.poster
  document.querySelectorAll(`[poster="${oldUrl}"],[poster="${newUrl}"]`)
    .forEach(function (e) {
      e.setAttribute("poster", newUrl);
    });

  if (button) {
    button.disabled = false;
    button.getElementsByClassName("fas")[0].classList.remove("fa-spin");
    button.style.display = null;
    if (!imgDate.url.endsWith(".svg")) {
      button.parentElement.querySelector(".age").dataset.age = new Date(imgDate.headers.get("Last-Modified")).getTime();
    }
  }
  return newUrl;
}

function refresh_imgs() {
  document.querySelectorAll(".refresh_img").forEach(async function (image) {
    let url = image.getAttribute(image.nodeName === "IMG" ? "src" : "poster");
    if (url === null) { return; }
    let CameraBattery = document.getElementById(image.dataset.cam).dataset.battery?.toLowerCase() == "true";
    let CameraConnected = image.classList.contains("connected");
    let CameraEnabled = image.classList.contains("enabled");
    await update_img(url, !(CameraEnabled && (!CameraBattery || CameraConnected)));
  });
}

document.addEventListener("DOMContentLoaded", () => {
  const grid = document.querySelector(".cameras");
  const selector = ".camera";
  sortable(grid, selector, function () {
    const cameras = document.querySelectorAll(selector);
    const ids = [...cameras].map((camera) => camera.id).filter((x) => x);
    const newOrdering = ids.join(",");
    console.debug("New camera_order", newOrdering);
    setCookie("camera_order", newOrdering);
    updateQueryParam("order", newOrdering)
  });
});

function updateQueryParam(paramName, paramValue) {
  let url = new URL(window.location.href);
  url.searchParams.set(paramName, paramValue);
  window.history.replaceState(null, null, url.toString().replaceAll("%2C", ","));
}
document.addEventListener("DOMContentLoaded", () => {
  // Filter cameras
  function filterCams() {
    document
      .querySelector("[data-filter].is-active")
      .classList.remove("is-active");
    this.classList.add("is-active");
    document.querySelectorAll("div.camera.is-hidden").forEach((div) => {
      div.classList.remove("is-hidden");
    });
    let filter = this.dataset.filter;
    if (filter != "all") {
      document
        .querySelectorAll("div.camera:not([data-" + filter + "='True'])")
        .forEach((cam) => {
          cam.classList.add("is-hidden");
        });
    }
  }
  document.querySelectorAll("a[data-filter]").forEach((a) => {
    a.addEventListener("click", filterCams);
  });
  document
    .querySelector(".navbar-brand .navbar-burger")
    .addEventListener("click", function () {
      this.classList.toggle("is-active");
      document.getElementById("refresh-menu").classList.toggle("is-active");
    });

  // Check for version update
  const checkAPI = document.getElementById("checkUpdate");
  checkAPI.addEventListener("click", () => {
    let icon = checkAPI.getElementsByClassName("fa-arrows-rotate")[0].classList;
    icon.add("fa-spin");
    fetch("https://api.github.com/repos/zachweyland/docker-wyze-bridge/releases/latest")
      .then((response) => response.json())
      .then((data) => {
        let apiVersion = data.tag_name.replace(/[^0-9\.]/g, "");
        if (apiVersion.localeCompare(checkAPI.dataset.version, undefined, { numeric: true }) === 1) {
          sendNotification('Update available!', `🎉 v${apiVersion}`, "warning");
        } else {
          sendNotification('All up to date!', '✅ Running the latest version!', "success");
        }
      })
      .catch((error) => { sendNotification('Update check failed', error.message, "danger") })
      .finally(() => { icon.remove("fa-spin"); });
  });

  // Update preview after loading the page
  async function loadPreview(img) {
    let cam = img.getAttribute("data-cam");
    var oldUrl = img.getAttribute("src");
    if (oldUrl == null || !oldUrl.includes(cam)) {
      oldUrl = `snapshot/${cam}.jpg`;
    }
    try {
      let newUrl = await update_img(oldUrl, (getCookie("refresh_period") <= 10 || !img.classList.contains("enabled")));
      let newVal = newUrl;
      img.parentElement.querySelectorAll("[src$=loading\\.svg],[style*=loading\\.svg],[poster$=loading\\.svg]")
        .forEach((e) => {
          for (let attr of e.attributes) {
            if (attr.value.includes("loading.svg")) {
              if (attr.name == "style") {
                newVal = `background-image: url(${newUrl});`;
              }
              e.setAttribute(attr.name, newVal);
              continue;
            }
          }
        });
      img.classList.remove("loading-preview");
    } catch {
      setTimeout(() => {
        loadPreview(img);
      }, 30000);
    }
  }
  document.querySelectorAll(".loading-preview").forEach(loadPreview);

  // click to update preview
  document.querySelectorAll(".update-preview[data-cam]").forEach((button) => {
    button.addEventListener("click", async () => {
      let img = document.querySelector(`.refresh_img[data-cam=${button.getAttribute("data-cam")}]`);
      let imgSrc = img.getAttribute(img.nodeName === "IMG" ? "src" : "poster");
      if (img && imgSrc) {
        await update_img(imgSrc);
      }
    });
  });

  // Restart bridge/rtsp-simple-server.
  document.querySelectorAll("#restart-menu a").forEach((a) => {
    a.addEventListener("click", (e) => {
      e.stopPropagation();
      a.style = "pointer-events: none;";
      a.classList.add("has-text-danger");
      fetch("restart/" + a.dataset.restart)
        .then((resp) => resp.json())
        .then((data) => { sendNotification(`Restart ${a.dataset.restart}`, data.result, "warning"); })
        .catch((error) => { sendNotification(`Restart ${a.dataset.restart}`, error.message, "danger"); });
      setTimeout(() => {
        a.style = null;
        a.classList.remove("has-text-danger");
      }, 3000);
    });
  });

  // Update status icon based on connection status
  const sse = new EventSource("api/sse_status");
  function markSseActivity() {
    sse_last_activity = Date.now();
    if (sse_error_timeout) {
      clearTimeout(sse_error_timeout);
      sse_error_timeout = null;
    }
  }
  sse.addEventListener("open", () => {
    markSseActivity();
    document.getElementById("connection-lost").style.display = "none";
    document.querySelectorAll(".cam-overlay button.offline").forEach((btn) => {
      btn.disabled = false;
      btn.classList.remove("offline");
      let icon = btn.getElementsByClassName("fas")[0]
      icon.classList.remove("fa-plug-circle-exclamation");
      icon.classList.add("fa-arrows-rotate");
    })
    autoplay();
    applyPreferences();
  });
  sse.addEventListener("ping", () => {
    markSseActivity();
  });
  sse.addEventListener("error", () => {
    if (sse_error_timeout) { return; }
    sse_error_timeout = setTimeout(() => {
      if (sse.readyState === EventSource.OPEN || Date.now() - sse_last_activity < 20000) {
        sse_error_timeout = null;
        return;
      }
      refresh_period = -1;
      clearInterval(refresh_interval);
      document.getElementById("connection-lost").style.display = "block";
      autoplay("stop");
      document.querySelectorAll("img.refresh_img,video[data-cam]").forEach((i) => { i.classList.remove("connected", "enabled") })
      document.querySelectorAll(".cam-overlay").forEach((i) => {
        i.getElementsByClassName("fas")[0].classList.remove("fa-spin");
      })
      document.querySelectorAll("[data-enabled=True] .card-header-title .status i").forEach((i) => {
        i.setAttribute("class", "fas fa-circle-exclamation")
      });
      document.querySelectorAll(".cam-overlay button").forEach((btn) => {
        btn.disabled = true;
        btn.parentElement.style.display = null;
        btn.classList.add("offline");
        let icon = btn.getElementsByClassName("fas")[0]
        icon.classList.remove("fa-arrows-rotate", "fa-spin");
        icon.classList.add("fa-plug-circle-exclamation");
      })
      sse_error_timeout = null;
    }, 5000);
  });
  sse.addEventListener("message", (event) => {
    markSseActivity();
    const data = JSON.parse(event.data);

    for (const [cam, messageData] of Object.entries(data)) {
      const card = document.getElementById(cam);
      const statusIcon = card.querySelector(".status i.fas");
      const preview = card.querySelector(`img.refresh_img, video[data-cam='${cam}']`);
      const motionIcon = card.querySelector(".icon.motion");
      const controlPanel = card.querySelector(".cam-control");
      const connected = card.dataset.connected.toLowerCase() === "true";
      updateBatteryLevel(card);

      updateControlButtons(
        controlPanel,
        "stream-enabled",
        messageData.status === "disabled" ? "disable" : "enable"
      );
      updateControlButtons(
        controlPanel,
        "stream-running",
        ["connected", "connecting"].includes(messageData.status) ? "start" : "stop"
      );

      card.dataset.connected = false;
      statusIcon.className = "fas";
      statusIcon.parentElement.title = "";

      if (preview) {
        preview.classList.toggle("connected", messageData.status === "connected");
        preview.classList.toggle("enabled", messageData.status !== "disabled");
      }

      if (messageData.motion) {
        motionIcon.classList.remove("is-hidden");
        sendNotification('Motion', `Motion detected on ${cam}`, "info");
      } else {
        motionIcon.classList.add("is-hidden");
      }

      switch (messageData.status) {
        case "connected":
          if (!connected) {
            sendNotification('Connected', `Connected to ${cam}`, "success");
          }
          card.dataset.connected = true;
          statusIcon.classList.add("fa-circle-play", "has-text-success");
          statusIcon.parentElement.title = "Click/tap to pause";
          autoplay();

          const noPreview = card.querySelector('.no-preview');
          if (noPreview) {
            const fig = noPreview.parentElement;
            const newPreview = document.createElement("img");
            newPreview.classList.add("refresh_img", "loading-preview", "connected");
            newPreview.dataset.cam = cam;
            newPreview.src = "static/loading.svg";
            fig.replaceChild(newPreview, noPreview);
            loadPreview(fig.querySelector("img"));
          }
          break;

        case "connecting":
          statusIcon.classList.add("fa-satellite-dish", "has-text-warning");
          statusIcon.parentElement.title = "Click/tap to pause";
          break;

        case "stopped":
          if (connected) {
            sendNotification('Disconnected', `Disconnected from ${cam}`, "danger");
          }
          statusIcon.classList.add("fa-circle-pause");
          statusIcon.parentElement.title = "Click/tap to play";
          break;

        case "offline":
          if (connected) {
            sendNotification('Offline', `${cam} is offline`, "danger");
          }
          statusIcon.classList.add("fa-ghost");
          statusIcon.parentElement.title = "Camera offline";
          break;

        default:
          if (connected) {
            sendNotification('Disconnected', `Disconnected from ${cam}`, "danger");
          }
          statusIcon.className = "fas fa-circle-exclamation";
          statusIcon.parentElement.title = "Not Connected";
          break;
      }
    }
  });

  // Toggle Camera details
  function appendDetailCell(cell, key, value) {
    if (typeof value === "string" && (key.endsWith("_url") || key.endsWith(" URL") || /^(https?|rtsp|rtmp):\/\//i.test(value))) {
      const link = document.createElement("a");
      link.href = value;
      link.title = value;
      link.textContent = value;
      cell.replaceChildren(link);
      return;
    }
    const code = document.createElement("code");
    code.textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2);
    cell.replaceChildren(code);
  }

  function insertDetailRow(table, key, value) {
    const newRow = table.insertRow();
    const keyCell = newRow.insertCell(0);
    const valCell = newRow.insertCell(1);
    keyCell.textContent = key;
    appendDetailCell(valCell, key, value);
  }

  const DETAIL_LABELS = {
    nickname: "Nickname",
    name_uri: "Name URI",
    status: "Status",
    connected: "Connected",
    enabled: "Enabled",
    on_demand: "On Demand",
    model_name: "Model Name",
    product_model: "Product Model",
    hls_url: "HLS URL",
    rtsp_url: "RTSP URL",
    rtmp_url: "RTMP URL",
    snapshot_url: "Snapshot URL",
    thumbnail_url: "Thumbnail URL",
    img_url: "Image URL",
    img_time: "Image Time",
    motion: "Motion",
    motion_ts: "Motion Timestamp",
    firmware_ver: "Firmware Version",
    timezone_name: "Timezone",
    req_bitrate: "Requested Bitrate",
    req_frame_size: "Requested Frame Size",
    is_2k: "2K",
    is_battery: "Battery Powered",
    rtsp_fw: "RTSP Firmware",
    rtsp_fw_enabled: "RTSP Firmware Enabled",
    basicInfo: "Basic Info",
    audioParm: "Audio",
    videoParm: "Video",
    settingParm: "Settings",
    channelResquestResult: "Channel Request",
    recordType: "Record Type",
    sdParm: "SD Card",
    uDiskParm: "USB Disk",
    apartalarmParm: "Alarm Zone",
    cameraParm: "Camera",
    floodlightParm: "Floodlight",
    motionDetectionParm: "Motion Detection",
    memoryCardParm: "Memory Card",
    sirenParm: "Siren",
    wifiParm: "WiFi",
    indicatorLightParm: "Indicator Light",
    controls: "Controls",
    sampleRate: "Sample Rate",
    firmware: "Firmware",
    hardware: "Hardware",
    model: "Model",
    type: "Type",
    wifidb: "WiFi Signal",
    audio: "Audio",
    video: "Video",
    capacity: "Capacity",
    free: "Free",
    status: "Status",
    location: "Location",
    nightVision: "Night Vision",
    osd: "OSD",
    stateVision: "Status Light",
    tz: "TZ Offset",
    bitRate: "Bitrate",
    fps: "FPS",
    horizontalFlip: "Horizontal Flip",
    verticalFlip: "Vertical Flip",
    resolution: "Resolution",
    logo: "Logo Watermark",
    time: "Time Watermark",
  };

  const TOP_LEVEL_DETAIL_ORDER = [
    "nickname",
    "name_uri",
    "status",
    "connected",
    "enabled",
    "on_demand",
    "model_name",
    "product_model",
    "firmware_ver",
    "ip",
    "mac",
    "timezone_name",
    "motion",
    "motion_ts",
    "hls_url",
    "rtsp_url",
    "rtmp_url",
    "snapshot_url",
    "thumbnail_url",
    "img_url",
    "img_time",
  ];

  function formatLabelPart(part) {
    const known = DETAIL_LABELS[part];
    if (known) { return known; }
    return part
      .replace(/_/g, " ")
      .replace(/-/g, " ")
      .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
      .replace(/\b\w/g, (m) => m.toUpperCase());
  }

  function formatDetailLabel(key) {
    return key.split(".").map(formatLabelPart).join(" / ");
  }

  function formatDetailValue(key, value) {
    if (typeof value === "boolean") {
      return value ? "On" : "Off";
    }
    if (key === "cameraParm.recording-mode" && value === 0) {
      return "Event only";
    }
    return value;
  }

  function pushCommonRows(data, rows) {
    TOP_LEVEL_DETAIL_ORDER.forEach((key) => {
      const value = data[key];
      if (value === null || value === undefined || value === "") { return; }
      rows.push([DETAIL_LABELS[key] || formatDetailLabel(key), formatDetailValue(key, value)]);
    });
  }

  function floodlightTriggerSummary(triggerSource) {
    if (typeof triggerSource !== "number") { return triggerSource; }
    const options = [
      [1, "Person"],
      [2, "Vehicle"],
      [4, "Pet"],
      [8, "Other"],
    ];
    const enabled = options.filter(([bit]) => (triggerSource & bit) === bit).map(([, label]) => label);
    return enabled.length ? enabled.join(", ") : "None";
  }

  function renderFloodlightDetails(table, data) {
    const info = data.camera_info || {};
    const basic = info.basicInfo || {};
    const floodlight = info.floodlightParm || {};
    const camera = info.cameraParm || {};
    const motion = info.motionDetectionParm || {};
    const wifi = info.wifiParm || {};
    const indicator = info.indicatorLightParm || {};

    const friendlyRows = [];
    pushCommonRows(data, friendlyRows);
    friendlyRows.push(
      ["Model", basic.model],
      ["Firmware", basic.firmware],
      ["Hardware", basic.hardware],
      ["IP", basic.ip],
      ["Location", basic.lat != null && basic.lon != null ? `${basic.lat}, ${basic.lon}` : null],
      ["Timezone", basic.timezone],
      ["HLS URL", data.hls_url],
      ["RTSP URL", data.rtsp_url],
      ["RTMP URL", data.rtmp_url],
      ["Snapshot URL", data.snapshot_url],
      ["Thumbnail URL", data.thumbnail_url],
      ["Image URL", data.img_url],
      ["Resolution", camera.resolution],
      ["Recording Mode", camera["recording-mode"] === 0 ? "Event only" : camera["recording-mode"]],
      ["Floodlight", floodlight.on ? "On" : "Off"],
      ["Ambient Light", floodlight["ambient-light-switch"] ? "On" : "Off"],
      ["Ambient Brightness", floodlight["ambient-light-brightness"]],
      ["Motion Light", floodlight["motion-activate-light-switch"] ? "On" : "Off"],
      ["Motion Brightness", floodlight["motion-activate-brightness"]],
      ["Light Duration", floodlight["light-on-duration"] ? `${floodlight["light-on-duration"]}s` : null],
      ["Trigger Sources", floodlightTriggerSummary(floodlight["trigger-source"])],
      ["Flash With Siren", floodlight["flash-with-siren"] ? "On" : "Off"],
      ["Indicator Light", indicator.on ? "On" : "Off"],
      ["Motion Sensitivity", motion["sensitivity-motion"]],
      ["Motion Tagging", motion["motion-tag"] ? "On" : "Off"],
      ["Motion Zone", motion["motion-zone"] ? "Configured" : "Off"],
      ["WiFi Signal", wifi["signal-strength"]],
      ["Rotate Angle", camera["rotate-angle"]],
      ["Logo Watermark", camera["logo-watermark"] ? "On" : "Off"],
      ["Time Watermark", camera["time-watermark"] ? "On" : "Off"],
      ["Sound Collection", camera["sound-collection-on"] ? "On" : "Off"],
    );

    friendlyRows.forEach(([key, value]) => {
      if (value !== null && value !== undefined && value !== "") {
        insertDetailRow(table, key, value);
      }
    });

  }

  function flattenObjectRows(prefix, value, rows) {
    if (value === null || value === undefined || value === "") { return; }
    if (Array.isArray(value)) {
      rows.push([formatDetailLabel(prefix), value.join(", ")]);
      return;
    }
    if (typeof value !== "object") {
      rows.push([formatDetailLabel(prefix), formatDetailValue(prefix, value)]);
      return;
    }
    for (const [key, child] of Object.entries(value)) {
      flattenObjectRows(`${prefix}.${key}`, child, rows);
    }
  }

  function renderStructuredCameraDetails(table, data) {
    const info = data.camera_info || {};
    const rows = [];

    pushCommonRows(data, rows);

    for (const [key, value] of Object.entries(info)) {
      flattenObjectRows(key, value, rows);
    }

    rows.forEach(([key, value]) => insertDetailRow(table, key, value));
  }

  function toggleDetails() {
    const cam = this.getAttribute("data-cam")
    const card = document.getElementById(cam);
    const img = card.getElementsByClassName("card-image")[0]
    const content = card.getElementsByClassName("content")[0]
    let icon = this.getElementsByClassName("fas")[0].classList
    if (icon.contains("fa-circle-info")) {
      icon.remove("fa-circle-info");
      icon.add("fa-circle-xmark");
    } else {
      icon.remove("fa-circle-xmark");
      icon.add("fa-circle-info");
    }
    if (content.classList.contains("is-hidden")) {
      const table = content.getElementsByTagName("table")[0]
      fetch(`api/${cam}`).then(resp => resp.json()).then(data => {
        table.innerHTML = ""
        if (data.product_model === "LD_CFP" && data.camera_info) {
          renderFloodlightDetails(table, data);
          return;
        }
        if (data.camera_info && typeof data.camera_info === "object") {
          renderStructuredCameraDetails(table, data);
          return;
        }
        for (const [key, value] of Object.entries(data)) {
          if (key == "camera_info" && value != null) {
            for (const [k, v] of Object.entries(value)) {
              insertDetailRow(table, k, v)
            }
            continue;
          }
          let newRow = table.insertRow();
          let keyCell = newRow.insertCell(0)
          let valCell = newRow.insertCell(1)
          keyCell.textContent = key
          if (typeof value === 'string' && (key.endsWith("_url") || key == 'thumbnail')) {
            let link = document.createElement('a');
            link.href = value;
            link.title = value;
            link.innerHTML = value.substring(0, Math.min(40, value.length)) + (value.length >= 40 ? "..." : "");
            valCell.appendChild(link)
          } else {
            appendDetailCell(valCell, key, value)
          }
        }
      }).catch(error => { console.error(error); });
    }
    img.classList.toggle("is-hidden");
    content.classList.toggle("is-hidden");
  }
  document.querySelectorAll(".toggle-details").forEach((btn) => {
    btn.addEventListener("click", toggleDetails);
  });
  // Play/pause on-demand
  function clickDemand() {
    const icon = this.querySelector("i.fas")
    const uri = this.getAttribute("data-cam");
    if (icon.matches(".fa-circle-play, .fa-satellite-dish")) {
      icon.setAttribute("class", "fas fa-circle-notch fa-spin")
      fetch(`api/${uri}/state/stop`)
      console.debug("pause " + uri)
    } else if (icon.matches(".fa-circle-pause, .fa-ghost")) {
      icon.setAttribute("class", "fas fa-circle-notch fa-spin")
      fetch(`api/${uri}/state/start`)
      console.debug("play " + uri)
    }
  }
  document.querySelectorAll(".status.enabled").forEach((span) => {
    span.addEventListener("click", clickDemand);
  });

  // Preview age
  function imgAge() {
    document.querySelectorAll("span.age[data-age]").forEach((span) => {
      timestamp = parseInt(span.dataset.age)
      if (timestamp) {
        var created = new Date(timestamp).getTime(),
          s = Math.floor((new Date() - created) / 1000),
          age = s < 60 ? `${s}s` : s < 3600 ? `+${Math.floor(s / 60)}m` : s < 86400 ? `+${Math.floor(s / 3600)}h` : `+${Math.floor(s / 86400)}d`
        span.textContent = age
      }
    })
    setTimeout(imgAge, 1000);
  }
  if (!getCookie("show_video")) { imgAge() }


  // fullscreen mode
  function toggleFullscreen(fs) {
    if (fs === undefined) {
      fs = getCookie("fullscreen");
    }
    let icon = document.querySelector(".fullscreen .fas");
    icon.classList.remove("fa-maximize", "fa-minimize")
    icon.classList.add(fs ? "fa-minimize" : "fa-maximize")
    document.querySelector(".section").style.padding = fs ? "1.5rem" : "";
    document.querySelectorAll(".fs-display-none").forEach((e) => {
      if (fs) { e.classList.add("fs-mode") } else { e.classList.remove("fs-mode") }
    })
    if (fs) { autoplay(); }
  }

  document.querySelector(".fullscreen button").addEventListener("click", () => {
    let fs = getCookie("fullscreen", false) ? "" : "1";
    setCookie("fullscreen", fs)
    toggleFullscreen(fs)
  })
  toggleFullscreen()

  function loadHLS(videoElement) {
    if (!videoElement.paused && videoElement.hls && videoElement.hls.media) {
      return;
    }
    const videoSrc = videoElement.dataset.src;
    videoElement.controls = true;
    videoElement.classList.remove("placeholder");
    if (Hls.isSupported()) {
      const hlsConfig = { maxLiveSyncPlaybackRate: 1.5, liveDurationInfinity: true, maxBufferHole: 5, nudgeMaxRetry: 20, liveSyncDurationCount: 0, liveMaxLatencyDurationCount: 6 };
      if (videoElement.hls && !videoElement.hls.destroyed) {
        videoElement.hls.destroy();
      }
      const hls = new Hls(hlsConfig);
      const parsedUrl = new URL(videoSrc);
      videoElement.hls = hls;
      if (parsedUrl.username && parsedUrl.password) {
        hls.config.xhrSetup = (xhr) => {
          xhr.setRequestHeader('Authorization', `Basic ${btoa(`${parsedUrl.username}:${parsedUrl.password}`)}`);
        };
      }
      hls.on(Hls.Events.ERROR, (evt, data) => {
        if (data.type !== Hls.ErrorTypes.NETWORK_ERROR || videoElement.classList.contains("connected")) {
          setTimeout(() => loadHLS(videoElement), 2000);
        } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR && videoElement.classList.contains("connected")) {
          console.debug("Media error", data.details);
        }
      });
      hls.on(Hls.Events.MEDIA_ATTACHED, () => {
        hls.loadSource(videoSrc);
        videoElement.muted = true;
        videoElement.play().catch((err) => {
          console.info('play() error:', err);
        });
      });
      hls.attachMedia(videoElement);
    } else if (videoElement.canPlayType('application/vnd.apple.mpegurl')) {
      console.log("loading mpeg")
      fetch(videoSrc)
        .then(() => { videoElement.src = videoSrc; })
        .catch(() => { loadHLS(videoElement); });
    }
  }

  document.querySelectorAll('video.hls.placeholder').forEach((videoElement) => {
    videoElement.parentElement.addEventListener("click", () => {
      loadHLS(videoElement);
      videoElement.play().catch((err) => {
        console.info('play() error:', err);
      });
    }, { "once": true });
    videoElement.addEventListener('play', () => {
      loadHLS(videoElement);
      if (!videoElement.classList.contains("connected") && !videoElement.hasAttribute("connecting")) {
        videoElement.setAttribute('connecting', '');
        fetch(`api/${videoElement.dataset.cam}/start`).then(() => {
          videoElement.classList.add("connected");
          videoElement.removeAttribute('connecting');
        }).catch((e) => {
          console.error('Error starting video stream:', e);
          videoElement.removeAttribute('connecting');
        });
      }
      videoElement.setAttribute('autoplay', '');
    });
    videoElement.addEventListener('pause', () => {
      videoElement.removeAttribute('autoplay');
    });
  });
  // Load WS for WebRTC on demand
  function loadWebRTC(video) {
    if (!video.classList.contains("placeholder")) { return }
    let videoFormat = getCookie("video");
    video.classList.remove("placeholder");
    video.controls = true;
    fetch(`signaling/${video.dataset.cam}?${videoFormat}`).then((resp) => resp.json()).then((data) => new Receiver(data));
  }
  // Click to load WebRTC

  document.querySelectorAll('[data-enabled=True] video.webrtc.placeholder').forEach((videoElement) => {
    videoElement.parentElement.addEventListener("click", () => { videoElement.play() }, { "once": true });
    videoElement.addEventListener("play", () => { loadWebRTC(videoElement) }, { "once": true });
    videoElement.addEventListener('pause', () => { videoElement.removeAttribute('autoplay'); });
  });
  // Auto-play video
  function autoplay(action) {
    let videos = document.querySelectorAll('video');
    if (action === "stop") {
      videos.forEach(video => {
        if (!video.paused) { video.classList.add("resume"); }
        video.pause();
        video.controls = false;
        video.classList.add("lost");
        video.removeAttribute('src');
        video.load();
      });
      return;
    }
    let autoPlay = getCookie("autoplay");
    let fullscreen = getCookie("fullscreen");
    videos.forEach(video => {
      const resume = video.classList.contains("resume")
      video.controls = true;
      video.classList.remove("lost");
      video.classList.remove("resume");
      if (!resume && !autoPlay && !fullscreen && !video.autoplay) { return }
      if (video.classList.contains("hls")) { loadHLS(video); }
      if (video.classList.contains("webrtc")) { loadWebRTC(video); }
      video.play().catch((err) => {
        console.info('play() error:', err);
      });
    });
  }
  // Change default video format for WebUI
  document.querySelectorAll(".preview-toggle [data-action]").forEach((e) => {
    e.addEventListener("click", () => {
      let videoCookie = getCookie("show_video")
      setCookie("show_video", "1");
      switch (e.dataset.action) {
        case "snapshot":
          setCookie("show_video", "");
          break;
        case "autoplay":
          let icon = e.querySelector("i.fas").classList;
          let playCookie = !!getCookie("autoplay");
          setCookie("autoplay", !playCookie);
          if (playCookie) { icon.replace("fa-square-check", "fa-square"); return; }
          icon.replace("fa-square", "fa-square-check")
          if (videoCookie) { autoplay(); return; }
          break;
        case "webrtc":
        case "hls":
        case "kvs":
          setCookie("video", e.dataset.action);
          break;
      }
      window.location = window.location.pathname;
    })
  })

  // cam control
  function updateControlValue(container, cmd, value) {
    const valueNode = container.querySelector(`[data-control-value="${cmd}"]`);
    if (!valueNode) { return; }
    if (typeof value === "boolean") {
      valueNode.dataset.state = value ? "on" : "off";
      valueNode.textContent = value ? "On" : "Off";
      return;
    }
    if (typeof value === "string") {
      const normalizedValue = value.trim().toLowerCase();
      if (["on", "off", "auto", "unknown"].includes(normalizedValue)) {
        valueNode.dataset.state = normalizedValue;
        valueNode.textContent = normalizedValue.charAt(0).toUpperCase() + normalizedValue.slice(1);
        return;
      }
    }
    delete valueNode.dataset.state;
    valueNode.textContent = value;
  }

  function updateControlButtons(container, group, payload) {
    if (!container) { return; }
    container.querySelectorAll(`.button[data-control-group="${group}"]`).forEach((btn) => {
      const isSelected = btn.dataset.payload === payload;
      btn.classList.toggle("is-selected", isSelected);
      btn.setAttribute("aria-pressed", isSelected ? "true" : "false");
    });
  }

  document.querySelectorAll(".cam-control").forEach((e) => {
    let { cam } = e.dataset;
    e.querySelectorAll(".button").forEach((button) => {
      button.addEventListener("click", () => {
        button.classList.add("is-loading");
        const { payload } = button.dataset
        fetch(`api/${cam}/${button.dataset.cmd}${payload ? `/${payload}` : ''}`)
          .then((resp) => resp.json())
          .then((data) => {
            if (data.status === "success" && button.dataset.controlGroup && payload) {
              updateControlButtons(e, button.dataset.controlGroup, payload);
            }
            if (data.status === "success" && ["floodlight", "ambient_light"].includes(button.dataset.cmd)) {
              updateControlValue(e, button.dataset.cmd, payload === "on");
            }
            if (data.status === "success" && button.dataset.cmd === "night_vision" && payload) {
              updateControlValue(e, "night_vision", payload);
            }
            sendNotification(cam, `${button.dataset.cmd}: ${data.status}`, ["error", false].includes(data.status) ? "danger" : "primary")
          })
          .catch((error) => { sendNotification(cam, `${button.dataset.cmd}: ${error.message}`, "danger") })
          .finally(() => { button.classList.remove("is-loading"); });
      })
    })
  })
  document.querySelectorAll(".cam-control-range").forEach((input) => {
    const container = input.closest(".cam-control");
    if (!container) { return; }
    updateControlValue(container, input.dataset.cmd, input.value);
    input.addEventListener("input", () => {
      updateControlValue(container, input.dataset.cmd, input.value);
    });
    input.addEventListener("change", () => {
      input.disabled = true;
      fetch(`api/${input.dataset.cam}/${input.dataset.cmd}/${input.value}`)
        .then((resp) => resp.json())
        .then((data) => {
          if (data.status === "success") {
            updateControlValue(container, input.dataset.cmd, data.value ?? input.value);
          }
          sendNotification(input.dataset.cam, `${input.dataset.cmd}: ${data.status}`, ["error", false].includes(data.status) ? "danger" : "primary")
        })
        .catch((error) => { sendNotification(input.dataset.cam, `${input.dataset.cmd}: ${error.message}`, "danger") })
        .finally(() => { input.disabled = false; });
    });
  });
  document.querySelectorAll(".drag_handle").forEach((e) => {
    e.addEventListener("mouseenter", () => { e.closest("div.card").classList.add("drag_hover") })
    e.addEventListener("mouseleave", () => { e.closest("div.card").classList.remove("drag_hover") })
  })

  function notificationEnabled() {
    if ("Notification" in window === false || window.isSecureContext === false) { return }
    if (Notification.permission === "granted") { return true }
    Notification.requestPermission().then((permission) => {
      if (permission === "granted") { return true }
    });
  }

  function sendNotification(title, message, type = "primary") {
    if (getCookie("fullscreen")) { return }
    if (notificationEnabled() === true && document.visibilityState != "visible") {
      new Notification(title, { body: message });
    } else {
      bulmaToast.toast({ message: `<strong>${title}</strong> - ${message}`, type: `is-${type}`, pauseOnHover: true, duration: 10000 })
    }
  }
  function updateBatteryLevel(card) {
    if (card.dataset.battery?.toLowerCase() !== "true") { return; }
    const iconElement = card.querySelector(".icon.battery i");
    fetch(`api/${card.id}/battery`)
      .then((resp) => resp.json())
      .then((data) => {
        if (data.status != "success" || !data.value) { return; }
        const batteryLevel = parseInt(data.value);
        let batteryIcon;
        iconElement.classList.remove("has-text-danger");
        iconElement.classList.forEach(cls => {
          if (cls.startsWith('fa-battery-')) {
            iconElement.classList.remove(cls);
          }
        });
        if (batteryLevel > 90) {
          batteryIcon = "full";
        } else if (batteryLevel > 75) {
          batteryIcon = "three-quarters";
        } else if (batteryLevel > 50) {
          batteryIcon = "half";
        } else if (batteryLevel > 10) {
          batteryIcon = "quarter";
        } else {
          batteryIcon = "empty";
          iconElement.classList.add("has-text-danger");
        }
        iconElement.classList.add(`fa-battery-${batteryIcon}`);
        iconElement.parentElement.title = `Battery Level: ${batteryLevel}%`;
      })
  }
  document.querySelectorAll('div.camera[data-battery="True"]').forEach((card) => {
    card.querySelector(".icon.battery").addEventListener("click", () => updateBatteryLevel(card));
  });
});
