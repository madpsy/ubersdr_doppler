/* map.js — Leaflet-based propagation path map for UberSDR Doppler Monitor
 *
 * Displays:
 *   • Day/night terminator (L.Terminator, updated every 60 s)
 *   • Receiver position (star marker)
 *   • Each non-reference station: transmitter marker + great-circle path
 *     + ionospheric reflection point markers (one per F-layer hop)
 *   • Popup details on every marker
 *
 * Public API (window.DopplerMap):
 *   init()           — create the map (called once on page load)
 *   update()         — redraw all station layers from current state
 *
 * Depends on:
 *   • Leaflet (global L)
 *   • L.Terminator (loaded from L.Terminator.js — L.terminator factory)
 *   • SunCalc (global SunCalc)
 *   • app.js globals: state, colourForIndex, maidenheadToLatLon,
 *                     greatCirclePoint, hopReflectionPoints, haversineKm,
 *                     estimateHopCount
 */
'use strict';

window.DopplerMap = (() => {

  // ── Internal state ──────────────────────────────────────────────────────────
  let map            = null;
  let terminator     = null;
  let rxMarker       = null;
  let stationLayers  = {};   // label → { txMarker, pathLine, reflMarkers[] }
  let terminatorTimer = null;
  let visible        = false;
  let initialised    = false;

  // ── Colour constants ─────────────────────────────────────────────────────────
  const NIGHT_FILL   = 'rgba(0, 10, 40, 0.45)';
  const NIGHT_STROKE = 'rgba(80, 140, 255, 0.6)';

  // ── Custom SVG icons ────────────────────────────────────────────────────────

  /** Receiver (star) icon */
  function rxIcon() {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="28" height="28" viewBox="-14 -14 28 28">
      <polygon points="0,-11 3,-4 11,-4 5,1 7,9 0,4 -7,9 -5,1 -11,-4 -3,-4"
               fill="#f0c040" stroke="#7a5c00" stroke-width="1.2"/>
    </svg>`;
    return L.divIcon({
      html: svg,
      className: '',
      iconSize: [28, 28],
      iconAnchor: [14, 14],
      popupAnchor: [0, -16],
    });
  }

  /** Transmitter (diamond) icon in the station colour */
  function txIcon(colour) {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="22" height="22" viewBox="-11 -11 22 22">
      <polygon points="0,-9 9,0 0,9 -9,0"
               fill="${colour}" stroke="#fff" stroke-width="1.5" opacity="0.92"/>
    </svg>`;
    return L.divIcon({
      html: svg,
      className: '',
      iconSize: [22, 22],
      iconAnchor: [11, 11],
      popupAnchor: [0, -13],
    });
  }

  /** Reflection point (circle) icon in the station colour */
  function reflIcon(colour, hopIndex, totalHops) {
    const r = 7;
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${r*2+4}" height="${r*2+4}" viewBox="-${r+2} -${r+2} ${(r+2)*2} ${(r+2)*2}">
      <circle r="${r}" fill="${colour}" fill-opacity="0.75" stroke="#fff" stroke-width="1.5"/>
      <text x="0" y="4" text-anchor="middle" font-size="7" font-family="monospace"
            font-weight="bold" fill="#fff">${hopIndex}/${totalHops}</text>
    </svg>`;
    return L.divIcon({
      html: svg,
      className: '',
      iconSize: [(r+2)*2, (r+2)*2],
      iconAnchor: [r+2, r+2],
      popupAnchor: [0, -(r+4)],
    });
  }

  // ── Great-circle polyline ───────────────────────────────────────────────────
  /** Build a dense array of [lat, lon] pairs along the great circle from a to b */
  function gcPolyline(a, b, steps = 120) {
    const pts = [];
    for (let i = 0; i <= steps; i++) {
      const p = greatCirclePoint(a, b, i / steps);
      pts.push([p.lat, p.lon]);
    }
    return pts;
  }

  // ── Terminator (L.Terminator.js — L.Polygon subclass) ──────────────────────
  //
  // Uses the bundled L.Terminator plugin which computes the day/night boundary
  // using proper solar position mathematics and renders it as a smooth
  // L.Polygon (Leaflet handles the curved rendering correctly).
  //
  function addTerminator() {
    terminator = L.terminator({
      fillColor:   NIGHT_FILL,
      fillOpacity: 0.45,
      color:       NIGHT_STROKE,
      weight:      1.5,
      opacity:     0.9,
      dashArray:   '4 3',
      interactive: false,
      resolution:  2,
    }).addTo(map);
  }

  function refreshTerminator() {
    if (!terminator) return;
    terminator.setTime(new Date());
  }

  // ── Receiver marker ─────────────────────────────────────────────────────────
  function updateRxMarker(rxGrid) {
    if (!rxGrid) return;
    const pos = maidenheadToLatLon(rxGrid);
    if (!pos) return;

    if (rxMarker) {
      rxMarker.setLatLng([pos.lat, pos.lon]);
    } else {
      rxMarker = L.marker([pos.lat, pos.lon], { icon: rxIcon(), zIndexOffset: 1000 })
        .bindPopup(`
          <div class="map-popup">
            <div class="map-popup-title">📡 Receiver</div>
            <div class="map-popup-row"><span class="map-popup-label">Grid</span><span>${rxGrid}</span></div>
            <div class="map-popup-row"><span class="map-popup-label">Lat/Lon</span><span>${pos.lat.toFixed(3)}°, ${pos.lon.toFixed(3)}°</span></div>
          </div>
        `, { maxWidth: 220 })
        .addTo(map);
    }
  }

  // ── Station layers ──────────────────────────────────────────────────────────
  function clearStationLayers() {
    Object.values(stationLayers).forEach(({ txMarker, pathLine, reflMarkers }) => {
      if (txMarker)    map.removeLayer(txMarker);
      if (pathLine)    map.removeLayer(pathLine);
      if (reflMarkers) reflMarkers.forEach(m => map.removeLayer(m));
    });
    stationLayers = {};
  }

  function buildStationLayers(stations, rxGrid) {
    if (!rxGrid) return;
    const rxPos = maidenheadToLatLon(rxGrid);
    if (!rxPos) return;

    stations.forEach((s, idx) => {
      if (!s.config || !s.config.grid || s.config.is_reference) return;
      const txPos = maidenheadToLatLon(s.config.grid);
      if (!txPos) return;

      const label   = s.config.label;
      const freqHz  = s.config.freq_hz || 10e6;
      const colour  = colourForIndex(idx);
      const distKm  = haversineKm(rxPos, txPos);
      const nHops   = estimateHopCount(distKm, freqHz);
      const reflPts = hopReflectionPoints(rxPos, txPos, freqHz);

      // ── Great-circle path ──
      const pathPts = gcPolyline(rxPos, txPos);
      const pathLine = L.polyline(pathPts, {
        color:     colour,
        weight:    2,
        opacity:   0.75,
        dashArray: '6 4',
        smoothFactor: 1,
      }).bindPopup(`
        <div class="map-popup">
          <div class="map-popup-title" style="color:${colour}">${label} — propagation path</div>
          <div class="map-popup-row"><span class="map-popup-label">Distance</span><span>${distKm.toFixed(0)} km</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Frequency</span><span>${(freqHz/1e6).toFixed(3)} MHz</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Est. hops</span><span>${nHops}</span></div>
        </div>
      `, { maxWidth: 220 }).addTo(map);

      // ── Transmitter marker ──
      const txMarker = L.marker([txPos.lat, txPos.lon], {
        icon: txIcon(colour),
        zIndexOffset: 500,
      }).bindPopup(`
        <div class="map-popup">
          <div class="map-popup-title" style="color:${colour}">📻 ${label}</div>
          <div class="map-popup-row"><span class="map-popup-label">Frequency</span><span>${(freqHz/1e6).toFixed(3)} MHz</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Grid</span><span>${s.config.grid}</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Lat/Lon</span><span>${txPos.lat.toFixed(3)}°, ${txPos.lon.toFixed(3)}°</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Distance</span><span>${distKm.toFixed(0)} km</span></div>
          <div class="map-popup-row"><span class="map-popup-label">Est. hops</span><span>${nHops}</span></div>
        </div>
      `, { maxWidth: 240 }).addTo(map);

      // ── Reflection point markers ──
      const reflMarkers = reflPts.map(({ lat, lon, hopIndex, totalHops }) => {
        // Compute sunrise/sunset at this reflection point for today
        const now = new Date();
        const times = SunCalc.getTimes(now, lat, lon);
        const srStr = times.sunrise && isFinite(times.sunrise.getTime())
          ? times.sunrise.toISOString().slice(11, 16) + ' UTC' : 'N/A';
        const ssStr = times.sunset && isFinite(times.sunset.getTime())
          ? times.sunset.toISOString().slice(11, 16) + ' UTC' : 'N/A';

        // Is this point currently in daylight?
        const sunAlt = SunCalc.getPosition(now, lat, lon).altitude;
        const isDaytime = sunAlt > 0;
        const dayNight = isDaytime ? '☀️ Daylight' : '🌙 Night';

        return L.marker([lat, lon], {
          icon: reflIcon(colour, hopIndex, totalHops),
          zIndexOffset: 200,
        }).bindPopup(`
          <div class="map-popup">
            <div class="map-popup-title" style="color:${colour}">${label} — hop ${hopIndex}/${totalHops} reflection</div>
            <div class="map-popup-row"><span class="map-popup-label">Lat/Lon</span><span>${lat.toFixed(2)}°, ${lon.toFixed(2)}°</span></div>
            <div class="map-popup-row"><span class="map-popup-label">Now</span><span>${dayNight}</span></div>
            <div class="map-popup-row"><span class="map-popup-label">☀️ Sunrise</span><span>${srStr}</span></div>
            <div class="map-popup-row"><span class="map-popup-label">🌙 Sunset</span><span>${ssStr}</span></div>
          </div>
        `, { maxWidth: 240 }).addTo(map);
      });

      stationLayers[label] = { txMarker, pathLine, reflMarkers };
    });
  }

  // ── Legend ──────────────────────────────────────────────────────────────────
  function buildLegend() {
    const legend = L.control({ position: 'bottomright' });
    legend.onAdd = () => {
      const div = L.DomUtil.create('div', 'map-legend');
      div.innerHTML = `
        <div class="map-legend-title">Legend</div>
        <div class="map-legend-row"><svg width="18" height="18" viewBox="-9 -9 18 18"><polygon points="0,-7 2.5,-3 7,-3 3.5,1 5,7 0,3 -5,7 -3.5,1 -7,-3 -2.5,-3" fill="#f0c040" stroke="#7a5c00" stroke-width="1"/></svg> Receiver</div>
        <div class="map-legend-row"><svg width="18" height="18" viewBox="-9 -9 18 18"><polygon points="0,-7 7,0 0,7 -7,0" fill="#58a6ff" stroke="#fff" stroke-width="1.2"/></svg> Transmitter</div>
        <div class="map-legend-row"><svg width="18" height="18" viewBox="-9 -9 18 18"><circle r="6" fill="#58a6ff" fill-opacity="0.75" stroke="#fff" stroke-width="1.2"/></svg> F-layer reflection</div>
        <div class="map-legend-row"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" stroke="#58a6ff" stroke-width="2" stroke-dasharray="5 3"/></svg> Path</div>
        <div class="map-legend-row"><svg width="28" height="8"><line x1="0" y1="4" x2="28" y2="4" stroke="rgba(80,140,255,0.6)" stroke-width="1.5" stroke-dasharray="4 3"/></svg> Terminator</div>
        <div class="map-legend-row"><span style="display:inline-block;width:14px;height:10px;background:rgba(0,10,40,0.45);border:1px solid rgba(80,140,255,0.4);vertical-align:middle;margin-right:4px"></span> Night</div>
      `;
      return div;
    };
    legend.addTo(map);
  }

  // ── UTC clock control ───────────────────────────────────────────────────────
  function buildClock() {
    const clock = L.control({ position: 'topright' });
    clock.onAdd = () => {
      const div = L.DomUtil.create('div', 'map-clock');
      div.id = 'map-utc-clock';
      div.title = 'Current UTC time';
      return div;
    };
    clock.addTo(map);
    function tick() {
      const el = document.getElementById('map-utc-clock');
      if (el) {
        const now = new Date();
        el.textContent = '🕐 ' + now.toISOString().slice(11, 19) + ' UTC';
      }
    }
    tick();
    setInterval(tick, 1000);
  }

  // ── Frequency readout control ───────────────────────────────────────────────
  let freqControl = null;

  function buildFreqReadout() {
    freqControl = L.control({ position: 'bottomleft' });
    freqControl.onAdd = () => {
      const div = L.DomUtil.create('div', 'map-freq-readout');
      div.id = 'map-freq-readout';
      return div;
    };
    freqControl.addTo(map);
  }

  function updateFreqReadout() {
    const el = document.getElementById('map-freq-readout');
    if (!el) return;
    const stations = (typeof state !== 'undefined') ? state.stations : [];
    if (!stations.length) { el.innerHTML = ''; return; }

    // Use the original station index (not a filtered index) so colours match
    // the chart/table/map markers which all call colourForIndex(originalIdx).
    const rows = [];
    stations.forEach((s, idx) => {
      if (!s.config || s.config.is_reference) return;
      const colour = colourForIndex(idx);
      const label  = s.config.label;
      const nomHz  = s.config.freq_hz || 0;
      const cur    = s.current;
      let freqStr  = '—';
      let doppStr  = '';
      if (cur && cur.valid) {
        // Prefer corrected Doppler; fall back to raw
        const dHz = (cur.corrected_doppler_hz !== null && cur.corrected_doppler_hz !== undefined)
          ? cur.corrected_doppler_hz : cur.doppler_hz;
        const absHz = nomHz + dHz;
        freqStr = absHz.toFixed(3) + ' Hz';
        const sign = dHz >= 0 ? '+' : '';
        doppStr = `<span class="map-freq-doppler">${sign}${dHz.toFixed(3)} Hz</span>`;
      }
      rows.push(`<div class="map-freq-row">
        <span class="map-freq-dot" style="background:${colour}"></span>
        <span class="map-freq-label">${label}</span>
        <span class="map-freq-value">${freqStr}</span>
        ${doppStr}
      </div>`);
    });

    el.innerHTML = rows.length
      ? `<div class="map-freq-title">Received frequency</div>${rows.join('')}`
      : '';
  }

  // ── Public API ──────────────────────────────────────────────────────────────

  function init() {
    if (initialised) return;
    initialised = true;
    visible = true;

    map = L.map('doppler-map', {
      center: [30, -40],
      zoom: 2,
      minZoom: 1,
      maxZoom: 8,
      worldCopyJump: true,
      zoomControl: true,
    });

    // CartoDB Dark Matter — matches the dark UI theme
    L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
      attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors &copy; <a href="https://carto.com/attributions">CARTO</a>',
      subdomains: 'abcd',
      maxZoom: 19,
    }).addTo(map);

    addTerminator();
    buildLegend();
    buildClock();
    buildFreqReadout();

    // Refresh terminator every 60 s
    terminatorTimer = setInterval(() => {
      refreshTerminator();
      // Also refresh reflection point day/night status
      update();
    }, 60 * 1000);

    // Invalidate size after a short delay to handle any layout reflow
    setTimeout(() => { if (map) map.invalidateSize(); }, 100);
  }

  function update() {
    if (!initialised || !map) return;
    const stations = (typeof state !== 'undefined') ? state.stations : [];
    const rxGrid   = (typeof state !== 'undefined') ? state.receiverGrid : null;

    updateRxMarker(rxGrid);
    clearStationLayers();
    buildStationLayers(stations, rxGrid);
    refreshTerminator();
    updateFreqReadout();
  }

  return { init, update, updateFreqReadout };
})();
