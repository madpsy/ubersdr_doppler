/* app.js — ubersdr_doppler web UI
 *
 * Three-panel chart layout:
 *   Panel 1: Doppler shift (Hz offset) OR absolute received frequency (Hz)
 *   Panel 2: SNR (dB)
 *   Panel 3: Signal power (dBFS)
 */
'use strict';

// BASE_PATH is injected by the Go server from the X-Forwarded-Prefix header.
// When served via UberSDR's addon proxy at /addon/doppler/, this will be
// "/addon/doppler" so all API calls are correctly prefixed.
const BASE = (window.BASE_PATH || '').replace(/\/$/, '');

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
const state = {
  stations: [],          // [{config, current, baseline_mean_hz, corrected_doppler_hz}]
  chartDate: '',         // YYYY-MM-DD UTC date filter for history chart ('' = live rolling window)
  dopplerChart: null,
  snrChart: null,
  powerChart: null,
  chartDatasets: {},     // label → {doppler: idx, snr: idx, power: idx}
  historyHours: 24,
  showSNR: true,
  showPower: true,
  showRef: false,    // show reference station on history charts (off by default)
  chartMode: 'doppler',  // 'doppler' | 'absolute'
  auth: {
    passwordConfigured: false,
    authenticated: false,
  },
  audioPlaying: null,    // label of station currently being previewed, or null
  audioElement: null,    // <audio> element for preview
};

// ---------------------------------------------------------------------------
// Audio preview — opens the analysis modal
// ---------------------------------------------------------------------------
window.toggleAudioPreview = function(label) {
  // If the modal is already open for this station, close it
  if (typeof AudioAnalysisModal !== 'undefined' && AudioAnalysisModal.isOpen() && AudioAnalysisModal.activeLabel() === label) {
    AudioAnalysisModal.close();
    state.audioPlaying = null;
    renderStatusTable();
    return;
  }

  // Close any existing modal/preview first
  if (typeof AudioAnalysisModal !== 'undefined' && AudioAnalysisModal.isOpen()) {
    AudioAnalysisModal.close();
  }

  state.audioPlaying = label;
  renderStatusTable();

  if (typeof AudioAnalysisModal !== 'undefined') {
    AudioAnalysisModal.open(label).catch(e => console.warn('audio modal open failed:', e));
  }
};

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------
async function checkAuthStatus() {
  try {
    const r = await apiFetch('/api/auth/status');
    const s = await r.json();
    state.auth.passwordConfigured = s.password_configured;
    state.auth.authenticated = s.authenticated;
    updateAuthUI();
  } catch (e) {
    console.warn('auth status check failed', e);
  }
}

function updateAuthUI() {
  const { passwordConfigured, authenticated } = state.auth;
  const authBtn   = document.getElementById('auth-btn');
  const logoutBtn = document.getElementById('logout-btn');
  const addBtn    = document.getElementById('add-btn');
  const settingsForm = document.getElementById('settings-form');

  // Show login button if password is configured but not authenticated
  if (authBtn)   authBtn.style.display   = (passwordConfigured && !authenticated) ? '' : 'none';
  if (logoutBtn) logoutBtn.style.display = (passwordConfigured && authenticated)  ? '' : 'none';

  // Write controls: visible only when authenticated (or no password configured)
  const canWrite = !passwordConfigured || authenticated;
  if (addBtn) addBtn.style.display = canWrite ? '' : 'none';
  if (settingsForm) {
    const submitBtn = settingsForm.querySelector('button[type="submit"]');
    if (submitBtn) submitBtn.style.display = canWrite ? '' : 'none';
  }
  // Station edit/remove buttons are rendered dynamically in renderStationList()
}

function openLoginModal() {
  document.getElementById('login-password').value = '';
  document.getElementById('login-error').style.display = 'none';
  document.getElementById('login-modal').classList.remove('hidden');
  document.getElementById('login-password').focus();
}

function closeLoginModal() {
  document.getElementById('login-modal').classList.add('hidden');
}

const COLOURS = [
  '#58a6ff', '#3fb950', '#f78166', '#d29922',
  '#bc8cff', '#39d353', '#ff7b72', '#ffa657',
];

// ---------------------------------------------------------------------------
// Global settings
// ---------------------------------------------------------------------------
async function loadSettings() {
  try {
    const r = await apiFetch('/api/settings');
    const s = await r.json();
    document.getElementById('s-callsign').value = s.callsign || '';
    document.getElementById('s-grid').value = s.grid || '';
    const offsetEl = document.getElementById('s-manual-offset');
    if (offsetEl) offsetEl.value = s.manual_offset_hz ?? 0;
    document.getElementById('s-freq-ref').value = s.frequency_reference || 'none';
    document.getElementById('s-ref-desc').value = s.reference_description || '';
    updateRefBanner(s.frequency_reference || 'none');
  } catch (e) {
    console.warn('settings load failed', e);
  }
}

function updateRefBanner(freqRef) {
  const el = document.getElementById('ref-quality-banner');
  if (!el) return;
  el.className = 'ref-banner';
  switch (freqRef) {
    case 'gpsdo':
      el.classList.add('banner-gpsdo');
      el.textContent = '✓ GPSDO — GPS-disciplined oscillator. Absolute Doppler values are accurate to ~0.01 Hz.';
      break;
    case 'reference_station':
      el.classList.add('banner-refstat');
      el.textContent = '⚡ Reference station correction active. Hardware clock drift is cancelled in real time using the reference station signal. Mark one station as "Reference station" above.';
      break;
    default:
      el.classList.add('banner-none');
      el.textContent = '⚠ No frequency reference — free-running oscillator. Doppler values include hardware clock offset. Data is qualitative (useful for observing events, not precise measurements).';
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function fmtHz(hz) {
  if (Math.abs(hz) >= 1e6) return (hz / 1e6).toFixed(3) + ' MHz';
  if (Math.abs(hz) >= 1e3) return (hz / 1e3).toFixed(3) + ' kHz';
  return hz + ' Hz';
}

function fmtDoppler(hz) {
  if (hz === null || hz === undefined) return '—';
  const sign = hz >= 0 ? '+' : '';
  return sign + hz.toFixed(3) + ' Hz';
}

function dopplerClass(hz) {
  if (Math.abs(hz) < 0.01) return 'doppler-zero';
  return hz > 0 ? 'doppler-pos' : 'doppler-neg';
}

function fmtUTC(ts) {
  if (!ts) return '—';
  return new Date(ts).toISOString().slice(11, 19) + ' UTC';
}

function colourForIndex(i) {
  return COLOURS[i % COLOURS.length];
}

function hasReference() {
  return state.stations.some(s => s.config && s.config.is_reference);
}

async function apiFetch(path, opts = {}) {
  const r = await fetch(BASE + path, opts);
  if (!r.ok) throw new Error(await r.text());
  return r;
}

// ---------------------------------------------------------------------------
// Connection status badge
// ---------------------------------------------------------------------------
function setConnStatus(status) {
  const el = document.getElementById('conn-status');
  el.className = 'conn-badge conn-' + status;
  el.textContent = {
    connected:    '⬤ Live',
    connecting:   '⬤ Connecting…',
    disconnected: '⬤ Disconnected',
  }[status] || '⬤ Unknown';
}

// ---------------------------------------------------------------------------
// Status table
// ---------------------------------------------------------------------------
function renderStatusTable() {
  const tbody = document.getElementById('status-tbody');
  const thead = document.querySelector('#status-table thead tr');
  const showRef = hasReference();

  // Show/hide the "Reference" chart checkbox based on whether a ref station exists
  const refLabel = document.getElementById('show-ref-label');
  if (refLabel) refLabel.style.display = showRef ? '' : 'none';

  if (thead) {
    thead.innerHTML = `
      <th>Station</th>
      <th>Frequency</th>
      <th>Doppler (raw)</th>
      ${showRef ? '<th>Doppler (corrected)</th>' : ''}
      <th>1h Baseline</th>
      <th>SNR</th>
      <th>Signal</th>
      <th>Noise Floor</th>
      <th>Updated (UTC)</th>
      <th>State</th>
      <th>Preview</th>`;
  }

  if (state.stations.length === 0) {
    const cols = showRef ? 11 : 10;
    tbody.innerHTML = `<tr><td colspan="${cols}" class="loading">No stations configured — add one below.</td></tr>`;
    return;
  }

  tbody.innerHTML = state.stations.map((s, i) => {
    const r = s.current || {};
    const valid = r.valid;
    const colour = colourForIndex(i);
    const isRef = s.config && s.config.is_reference;
    const label = s.config.label;

    const dHz   = valid ? fmtDoppler(r.doppler_hz) : '—';
    const cls   = valid ? dopplerClass(r.doppler_hz) : 'invalid';
    const snr   = valid ? r.snr_db.toFixed(1) + ' dB' : '—';
    const sig   = valid ? r.signal_dbfs.toFixed(1) + ' dBFS' : '—';
    const noise = valid ? r.noise_dbfs.toFixed(1) + ' dBFS' : '—';
    const ts    = r.timestamp ? fmtUTC(r.timestamp) : '—';

    let corrCell = '';
    if (showRef) {
      if (isRef) {
        corrCell = '<td style="color:var(--muted)">— (ref)</td>';
      } else if (s.corrected_doppler_hz !== null && s.corrected_doppler_hz !== undefined) {
        const cHz = s.corrected_doppler_hz;
        corrCell = `<td class="${dopplerClass(cHz)}">${fmtDoppler(cHz)}</td>`;
      } else {
        corrCell = '<td class="invalid">—</td>';
      }
    }

    let baselineCell = '<td style="color:var(--muted)">—</td>';
    if (s.baseline_mean_hz !== null && s.baseline_mean_hz !== undefined) {
      baselineCell = `<td style="color:var(--muted)">${fmtDoppler(s.baseline_mean_hz)}</td>`;
    }

    let stateTxt = '<span class="state-nosig">No signal</span>';
    if (valid) {
      if (r.snr_db >= 20) stateTxt = '<span class="state-ok">Good</span>';
      else if (r.snr_db >= 10) stateTxt = '<span class="state-weak">Weak</span>';
      else stateTxt = '<span class="state-weak">Marginal</span>';
    }

    const refBadge = isRef ? ' <span class="ref-badge">REF</span>' : '';
    const isPlaying = state.audioPlaying === label;
    const previewBtn = `<button class="btn btn-secondary btn-sm" onclick="toggleAudioPreview('${label}')">${isPlaying ? '⏹ Stop' : '▶ Listen'}</button>`;

    return `<tr>
      <td><span class="station-dot" style="background:${colour}"></span><strong>${label}</strong>${refBadge}</td>
      <td>${fmtHz(s.config.freq_hz)}</td>
      <td class="${cls}">${dHz}</td>
      ${corrCell}
      ${baselineCell}
      <td>${snr}</td>
      <td>${sig}</td>
      <td>${noise}</td>
      <td>${ts}</td>
      <td>${stateTxt}</td>
      <td>${previewBtn}</td>
    </tr>`;
  }).join('');
}

// ---------------------------------------------------------------------------
// Mini spectrum display — one canvas per station showing the 1 kHz window
// Matches UberSDR's frequency reference monitor display style.
// Supports mouse-wheel zoom and click-drag pan.
// ---------------------------------------------------------------------------

// Per-canvas zoom/pan state: { centerBin, halfSpan }
// centerBin: the bin index at the centre of the view (default n/2)
// halfSpan:  half the number of bins visible (default n/2 = full view)
const specViewState = {};

// Set to true whenever zoom/pan changes so the rAF loop redraws.
let specDirty = false;

function startSpecRenderLoop() {
  function loop() {
    if (specDirty) {
      specDirty = false;
      state.stations.forEach((s, i) => {
        const label = s.config.label;
        const canvasId = `spec-canvas-${label.replace(/[^a-zA-Z0-9]/g, '_')}`;
        drawMiniSpectrum(canvasId, s, i);
      });
    }
    requestAnimationFrame(loop);
  }
  requestAnimationFrame(loop);
}

function getSpecView(canvasId, n) {
  if (!specViewState[canvasId]) {
    specViewState[canvasId] = { centerBin: n / 2, halfSpan: n / 2 };
  }
  return specViewState[canvasId];
}

function attachSpecInteraction(canvas, canvasId, getN) {
  // Prevent duplicate listeners
  if (canvas._specInteractionAttached) return;
  canvas._specInteractionAttached = true;

  // Mouse wheel → zoom (zoom towards cursor position)
  canvas.addEventListener('wheel', e => {
    e.preventDefault();
    const n = getN();
    if (!n) return;
    const view = getSpecView(canvasId, n);
    const ML = 44;
    // Use CSS pixel dimensions (clientWidth) so coords match e.clientX
    const plotW = canvas.clientWidth - ML;
    const rect = canvas.getBoundingClientRect();
    const cursorFrac = Math.max(0, Math.min(1, (e.clientX - rect.left - ML) / plotW));
    const cursorBin = (view.centerBin - view.halfSpan) + cursorFrac * view.halfSpan * 2;
    const factor = e.deltaY < 0 ? 0.7 : 1.4;
    const newHalfSpan = Math.max(5, Math.min(n / 2, view.halfSpan * factor));
    // Keep cursor bin stationary: newCenter so cursorBin stays at cursorFrac
    view.centerBin = cursorBin + (0.5 - cursorFrac) * newHalfSpan * 2;
    view.halfSpan = newHalfSpan;
    // Clamp
    view.centerBin = Math.max(view.halfSpan, Math.min(n - view.halfSpan, view.centerBin));
    specDirty = true;
  }, { passive: false });

  // Click-drag → pan
  let dragStart = null;
  let dragCenter = null;
  canvas.addEventListener('mousedown', e => {
    dragStart = e.clientX;
    const n = getN();
    if (n) dragCenter = getSpecView(canvasId, n).centerBin;
  });
  canvas.addEventListener('mousemove', e => {
    if (dragStart === null) return;
    const n = getN();
    if (!n) return;
    const view = getSpecView(canvasId, n);
    const ML = 44;
    // Use CSS pixel dimensions for drag distance
    const plotW = canvas.clientWidth - ML;
    const binsPerPx = (view.halfSpan * 2) / plotW;
    const dx = e.clientX - dragStart;
    // Invert: drag right → pan left (lower bins)
    view.centerBin = Math.max(view.halfSpan,
      Math.min(n - view.halfSpan, dragCenter - dx * binsPerPx));
    specDirty = true;
  });
  const endDrag = () => { dragStart = null; dragCenter = null; };
  canvas.addEventListener('mouseup', endDrag);
  canvas.addEventListener('mouseleave', endDrag);

  // Double-click → reset zoom
  canvas.addEventListener('dblclick', () => {
    const n = getN();
    if (!n) return;
    specViewState[canvasId] = { centerBin: n / 2, halfSpan: n / 2 };
    specDirty = true;
  });
}

function drawMiniSpectra() {
  const container = document.getElementById('mini-spectra');
  if (!container) return;

  if (state.stations.length === 0) {
    container.innerHTML = '<p style="color:var(--muted)">No stations configured.</p>';
    return;
  }

  // Create or reuse canvas elements
  state.stations.forEach((s, i) => {
    const label = s.config.label;
    const canvasId = `spec-canvas-${label.replace(/[^a-zA-Z0-9]/g, '_')}`;
    let wrapper = document.getElementById(`spec-wrap-${canvasId}`);
    if (!wrapper) {
      wrapper = document.createElement('div');
      wrapper.id = `spec-wrap-${canvasId}`;
      wrapper.style.cssText = 'margin-bottom:16px';
      wrapper.innerHTML = `
        <div style="font-size:0.82rem;color:var(--muted);margin-bottom:4px">
          <span class="station-dot" style="background:${colourForIndex(i)};display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px;vertical-align:middle"></span>
          <strong style="color:var(--text)">${label}</strong>
          <span style="margin-left:8px">${fmtHz(s.config.freq_hz)}</span>
          <span id="spec-info-${canvasId}" style="margin-left:12px;color:var(--accent)"></span>
        </div>
        <canvas id="${canvasId}" width="500" height="100" style="width:100%;height:100px;background:var(--bg);border-radius:4px;border:1px solid var(--border);cursor:crosshair"></canvas>`;
      container.appendChild(wrapper);
    }
    const canvas = document.getElementById(canvasId);
    const n = s.spectrum_data ? s.spectrum_data.length : 500;
    attachSpecInteraction(canvas, canvasId, () => s.spectrum_data ? s.spectrum_data.length : 0);
    drawMiniSpectrum(canvasId, s, i);
  });
}

function drawMiniSpectrum(canvasId, s, stationIdx) {
  const canvas = document.getElementById(canvasId);
  if (!canvas) return;
  const ctx = canvas.getContext('2d');

  // Match canvas buffer to its CSS display size × devicePixelRatio so text
  // and lines are crisp and not stretched on any screen width.
  const dpr = window.devicePixelRatio || 1;
  const cssW = canvas.clientWidth  || 500;
  const cssH = canvas.clientHeight || 100;
  const W = Math.round(cssW * dpr);
  const H = Math.round(cssH * dpr);
  if (canvas.width !== W || canvas.height !== H) {
    canvas.width  = W;
    canvas.height = H;
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  // From here all coordinates are in CSS pixels (cssW × cssH logical space)
  const CW = cssW;
  const CH = cssH;

  // Layout margins for axes
  const ML = 44; // left margin for dBFS labels
  const MB = 18; // bottom margin for Hz labels
  const plotW = CW - ML;
  const plotH = CH - MB;

  ctx.clearRect(0, 0, CW, CH);
  ctx.fillStyle = '#0d1117';
  ctx.fillRect(0, 0, CW, CH);

  const spectrum = s.spectrum_data;
  const peakBin = s.peak_bin !== undefined ? s.peak_bin : -1;
  const infoEl = document.getElementById(`spec-info-${canvasId}`);

  if (!spectrum || spectrum.length === 0) {
    ctx.fillStyle = '#8b949e';
    ctx.font = '11px sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('Waiting for spectrum data…', ML + plotW / 2, CH / 2);
    if (infoEl) infoEl.textContent = '';
    return;
  }

  const n = spectrum.length;
  const hzPerBin = s.bin_bandwidth > 0 ? s.bin_bandwidth : 50; // actual Hz/bin from server

  // ── Zoom/pan view window ──────────────────────────────────────────────────
  const view = getSpecView(canvasId, n);
  // Clamp in case n changed
  view.halfSpan = Math.max(10, Math.min(n / 2, view.halfSpan));
  view.centerBin = Math.max(view.halfSpan, Math.min(n - view.halfSpan, view.centerBin));
  const binStart = view.centerBin - view.halfSpan;
  const binEnd   = view.centerBin + view.halfSpan;

  // Helper: bin index → X pixel (maps [binStart, binEnd] → [ML, ML+plotW])
  const binToX = b => ML + ((b - binStart) / (binEnd - binStart)) * plotW;

  // Compute dBFS range over the visible window only
  let minVal = Infinity, maxVal = -Infinity;
  const iBinStart = Math.max(0, Math.floor(binStart));
  const iBinEnd   = Math.min(n - 1, Math.ceil(binEnd));
  for (let i = iBinStart; i <= iBinEnd; i++) {
    if (spectrum[i] < minVal) minVal = spectrum[i];
    if (spectrum[i] > maxVal) maxVal = spectrum[i];
  }
  if (!isFinite(minVal)) { minVal = -140; maxVal = -80; }
  // Round to nearest 10 dB for clean axis labels
  const dbMin = Math.floor(minVal / 10) * 10;
  const dbMax = Math.ceil(maxVal / 10) * 10;
  const dbRange = dbMax - dbMin || 10;

  // Helper: dBFS value → Y pixel
  const dbToY = db => plotH - ((db - dbMin) / dbRange) * plotH;

  // ── Draw spectrum bars (visible window only) ──────────────────────────────
  const colour = colourForIndex(stationIdx);
  ctx.fillStyle = colour + '88';
  const barW = Math.max(1, plotW / (binEnd - binStart));
  for (let i = iBinStart; i <= iBinEnd; i++) {
    const x = binToX(i);
    const y = dbToY(spectrum[i]);
    ctx.fillRect(x, y, barW, plotH - y);
  }

  // ── Y axis (dBFS) ─────────────────────────────────────────────────────────
  ctx.font = '9px sans-serif';
  ctx.textAlign = 'right';
  const dbStep = dbRange <= 20 ? 5 : dbRange <= 40 ? 10 : 20;
  for (let db = dbMin; db <= dbMax; db += dbStep) {
    const y = dbToY(db);
    if (y < 0 || y > plotH) continue;
    ctx.fillStyle = '#8b949e';
    ctx.fillText(db + ' dB', ML - 3, y + 3);
    ctx.strokeStyle = '#21262d';
    ctx.lineWidth = 1;
    ctx.setLineDash([2, 2]);
    ctx.beginPath();
    ctx.moveTo(ML, y);
    ctx.lineTo(CW, y);
    ctx.stroke();
    ctx.setLineDash([]);
  }

  // ── X axis (Hz offset from carrier) ──────────────────────────────────────
  // Compute visible Hz range and pick sensible label spacing
  const hzStart = (binStart - n / 2) * hzPerBin;
  const hzEnd   = (binEnd   - n / 2) * hzPerBin;
  const hzSpan  = hzEnd - hzStart;
  // Pick label step: aim for ~4–6 labels
  const rawStep = hzSpan / 5;
  const magnitude = Math.pow(10, Math.floor(Math.log10(Math.abs(rawStep) || 1)));
  const niceSteps = [1, 2, 5, 10, 20, 50, 100, 200, 500];
  let hzStep = (niceSteps.find(v => v * magnitude >= rawStep) || 50) * magnitude;
  if (hzStep < 1) hzStep = 1;

  ctx.font = '9px sans-serif';
  ctx.textAlign = 'center';
  const firstLabel = Math.ceil(hzStart / hzStep) * hzStep;
  for (let hz = firstLabel; hz <= hzEnd; hz += hzStep) {
    const bin = n / 2 + hz / hzPerBin;
    const x = binToX(bin);
    if (x < ML || x > CW) continue;
    ctx.fillStyle = '#8b949e';
    ctx.fillText((hz >= 0 ? '+' : '') + hz, x, CH - 3);
    ctx.strokeStyle = '#21262d';
    ctx.lineWidth = 1;
    ctx.setLineDash([2, 2]);
    ctx.beginPath();
    ctx.moveTo(x, 0);
    ctx.lineTo(x, plotH);
    ctx.stroke();
    ctx.setLineDash([]);
  }
  ctx.textAlign = 'right';
  ctx.fillStyle = '#555';
  ctx.fillText('Hz', CW - 2, CH - 3);

  // ── Centre line (nominal carrier — green dashed) ──────────────────────────
  const centreX = binToX(n / 2);
  if (centreX >= ML && centreX <= CW) {
    ctx.strokeStyle = '#3fb950';
    ctx.lineWidth = 1;
    ctx.setLineDash([3, 3]);
    ctx.beginPath();
    ctx.moveTo(centreX, 0);
    ctx.lineTo(centreX, plotH);
    ctx.stroke();
    ctx.setLineDash([]);
  }

  // ── Peak marker (red solid) ───────────────────────────────────────────────
  // Draw the marker at the sub-bin interpolated position derived from doppler_hz
  // rather than the integer peakBin, so it accurately reflects the measured frequency.
  const hasCurrent = s.current && s.current.valid;
  if (hasCurrent) {
    // Sub-bin centroid position: centre_bin + doppler_hz / hzPerBin
    const subBinPos = n / 2 + s.current.doppler_hz / hzPerBin;
    const peakX = binToX(subBinPos);
    if (peakX >= ML && peakX <= CW) {
      ctx.strokeStyle = '#f85149';
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(peakX, 0);
      ctx.lineTo(peakX, plotH);
      ctx.stroke();
    }
    const dHz = s.current.doppler_hz;
    const sign = dHz >= 0 ? '+' : '';
    if (infoEl) {
      infoEl.textContent = `${sign}${dHz.toFixed(3)} Hz  SNR: ${s.current.snr_db.toFixed(1)} dB`;
      infoEl.style.color = Math.abs(dHz) < 0.5 ? 'var(--green)' : 'var(--accent)';
    }
  } else if (peakBin >= 0 && peakBin < n) {
    // No valid reading yet but we have a peak bin — draw marker at integer bin
    const peakX = binToX(peakBin);
    if (peakX >= ML && peakX <= CW) {
      ctx.strokeStyle = '#f85149';
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(peakX, 0);
      ctx.lineTo(peakX, plotH);
      ctx.stroke();
    }
    if (infoEl) {
      infoEl.textContent = 'No signal';
      infoEl.style.color = 'var(--muted)';
    }
  } else {
    if (infoEl) {
      infoEl.textContent = 'No signal';
      infoEl.style.color = 'var(--muted)';
    }
  }

  // ── Axis border ───────────────────────────────────────────────────────────
  ctx.strokeStyle = '#30363d';
  ctx.lineWidth = 1;
  ctx.setLineDash([]);
  ctx.strokeRect(ML, 0, plotW, plotH);
}

// ---------------------------------------------------------------------------
// Charts — three panels matching HamSCI Grape paper layout
// ---------------------------------------------------------------------------

function xAxisConfig(showTitle) {
  return {
    type: 'time',
    time: {
      unit: 'minute',
      displayFormats: { minute: 'HH:mm', hour: 'HH:mm' },
      tooltipFormat: 'HH:mm:ss',
    },
    ticks: { color: '#8b949e', maxTicksLimit: 10, source: 'auto' },
    grid: { color: '#21262d' },
    title: showTitle
      ? { display: true, text: 'UTC time', color: '#8b949e', font: { size: 11 } }
      : { display: false },
  };
}

function initCharts() {
  // ── Panel 1: Doppler / absolute frequency ─────────────────────────────────
  const dCtx = document.getElementById('doppler-chart').getContext('2d');
  state.dopplerChart = new Chart(dCtx, {
    type: 'line',
    data: { datasets: [] },
    options: {
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      scales: {
        x: xAxisConfig(false),
        y: {
          id: 'y',
          title: { display: true, text: 'Doppler shift (Hz)', color: '#8b949e', font: { size: 11 } },
          ticks: { color: '#8b949e' },
          grid: { color: '#21262d' },
          afterDataLimits(scale) {
            if (state.chartMode === 'doppler') {
              if (scale.max < 0.5) scale.max = 0.5;
              if (scale.min > -0.5) scale.min = -0.5;
            }
          },
        },
      },
      plugins: {
        legend: {
          labels: {
            color: '#e6edf3', boxWidth: 12,
            // Hide the min/max band datasets from the legend
            filter: item => !item.text.endsWith(' min') && !item.text.endsWith(' max'),
          },
        },
        tooltip: {
          filter: item => !item.dataset._isBand,
          callbacks: {
            title: items => items.length ? new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC' : '',
            label: ctx => {
              // Skip band datasets in tooltip
              if (ctx.dataset._isBand) return null;
              const pt = ctx.raw;
              const val = state.chartMode === 'absolute'
                ? `${ctx.parsed.y.toFixed(3)} Hz`
                : fmtDoppler(ctx.parsed.y);
              const lines = [`${ctx.dataset.label}: ${val}`];
              if (pt && pt.std !== undefined && pt.std !== null) {
                lines.push(`  σ (jitter): ±${pt.std.toFixed(3)} Hz`);
              }
              if (pt && pt.min !== undefined && pt.max !== undefined && pt.min !== null) {
                lines.push(`  min: ${fmtDoppler(pt.min)}  max: ${fmtDoppler(pt.max)}`);
              }
              if (pt && pt.count !== undefined) {
                lines.push(`  samples: ${pt.count}`);
              }
              return lines;
            },
          },
        },
      },
    },
    plugins: [{
      id: 'zeroLine',
      afterDraw(chart) {
        const yScale = chart.scales.y;
        const xScale = chart.scales.x;
        if (!yScale || !xScale) return;
        // In doppler mode: draw zero line. In absolute mode: draw nominal freq lines.
        const ctx = chart.ctx;
        ctx.save();
        ctx.strokeStyle = 'rgba(255,255,255,0.25)';
        ctx.lineWidth = 1;
        ctx.setLineDash([4, 4]);
        if (state.chartMode === 'doppler') {
          const y = yScale.getPixelForValue(0);
          ctx.beginPath();
          ctx.moveTo(xScale.left, y);
          ctx.lineTo(xScale.right, y);
          ctx.stroke();
        } else {
          // Draw one nominal frequency line per station
          state.stations.forEach(s => {
            if (!s.config) return;
            const y = yScale.getPixelForValue(s.config.freq_hz);
            if (y >= yScale.top && y <= yScale.bottom) {
              ctx.beginPath();
              ctx.moveTo(xScale.left, y);
              ctx.lineTo(xScale.right, y);
              ctx.stroke();
            }
          });
        }
        ctx.restore();
      },
    }],
  });

  // ── Panel 2: SNR ──────────────────────────────────────────────────────────
  const sCtx = document.getElementById('snr-chart').getContext('2d');
  state.snrChart = new Chart(sCtx, {
    type: 'line',
    data: { datasets: [] },
    options: {
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      scales: {
        x: xAxisConfig(false),
        y: {
          title: { display: true, text: 'SNR (dB)', color: '#8b949e', font: { size: 11 } },
          ticks: { color: '#8b949e' },
          grid: { color: '#21262d' },
          min: 0,
        },
      },
      plugins: {
        legend: { display: false },
        tooltip: {
          callbacks: {
            title: items => items.length ? new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC' : '',
            label: ctx => `${ctx.dataset.label} SNR: ${ctx.parsed.y.toFixed(1)} dB`,
          },
        },
      },
    },
  });

  // ── Panel 3: Signal power (dBFS) ──────────────────────────────────────────
  const pCtx = document.getElementById('power-chart').getContext('2d');
  state.powerChart = new Chart(pCtx, {
    type: 'line',
    data: { datasets: [] },
    options: {
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      scales: {
        x: xAxisConfig(true),
        y: {
          title: { display: true, text: 'Signal power (dBFS)', color: '#8b949e', font: { size: 11 } },
          ticks: { color: '#8b949e' },
          grid: { color: '#21262d' },
        },
      },
      plugins: {
        legend: { display: false },
        tooltip: {
          callbacks: {
            title: items => items.length ? new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC' : '',
            label: ctx => `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(1)} dBFS`,
          },
        },
      },
    },
  });
}

// Convert doppler_hz to chart Y value based on current mode
function dopplerToY(dopplerHz, nominalFreqHz) {
  if (state.chartMode === 'absolute') {
    return nominalFreqHz + dopplerHz;
  }
  return dopplerHz;
}

function updateDopplerChartAxis() {
  const chart = state.dopplerChart;
  if (!chart) return;
  const yAxis = chart.options.scales.y;
  const corrected = hasReference();
  if (state.chartMode === 'absolute') {
    yAxis.title.text = 'Received frequency (Hz)';
  } else if (corrected) {
    yAxis.title.text = 'Doppler shift — corrected (Hz)';
  } else {
    yAxis.title.text = 'Doppler shift (Hz)';
  }
  chart.update('none');
}

async function loadHistory() {
  // When a specific date is selected, fetch that UTC day; otherwise use rolling window.
  const usingDate = state.chartDate !== '';
  const cutoff = usingDate ? 0 : Date.now() - state.historyHours * 3600 * 1000;
  state.dopplerChart.data.datasets = [];
  state.snrChart.data.datasets = [];
  state.powerChart.data.datasets = [];
  state.chartDatasets = {};

  for (let i = 0; i < state.stations.length; i++) {
    const s = state.stations[i];
    // Skip reference station unless the user has opted in
    if (s.config.is_reference && !state.showRef) continue;
    const label = s.config.label;
    const nominalHz = s.config.freq_hz;
    const colour = colourForIndex(i);
    try {
      const url = usingDate
        ? `/api/history?station=${encodeURIComponent(label)}&date=${encodeURIComponent(state.chartDate)}`
        : `/api/history?station=${encodeURIComponent(label)}`;
      const r = await apiFetch(url);
      const history = await r.json() || [];
      const filtered = usingDate
        ? history  // server already filtered to the day
        : history.filter(m => new Date(m.timestamp).getTime() >= cutoff);

      // Use corrected Doppler when available (reference station offset applied)
      const dopplerPoints = filtered.map(m => {
        const hz = (m.corrected_doppler_hz !== null && m.corrected_doppler_hz !== undefined)
          ? m.corrected_doppler_hz : m.doppler_hz;
        return {
          x: new Date(m.timestamp),
          y: dopplerToY(hz, nominalHz),
          // Attach scientific stats for tooltip
          min: m.min_doppler_hz,
          max: m.max_doppler_hz,
          std: m.std_dev_hz,
          count: m.count,
        };
      });
      // Min/max band: lower boundary dataset (filled up to the mean line)
      const dopplerMinPoints = filtered.map(m => ({
        x: new Date(m.timestamp),
        y: m.min_doppler_hz !== undefined ? dopplerToY(m.min_doppler_hz, nominalHz) : null,
      }));
      const dopplerMaxPoints = filtered.map(m => ({
        x: new Date(m.timestamp),
        y: m.max_doppler_hz !== undefined ? dopplerToY(m.max_doppler_hz, nominalHz) : null,
      }));

      const snrPoints     = filtered.map(m => ({ x: new Date(m.timestamp), y: m.snr_db }));
      const powerPoints   = filtered.map(m => ({ x: new Date(m.timestamp), y: m.signal_dbfs }));

      // Push min band (lower boundary), then max band (upper boundary, fills down to min)
      const dMinIdx = state.dopplerChart.data.datasets.length;
      state.dopplerChart.data.datasets.push({
        label: label + ' min',
        data: dopplerMinPoints,
        borderColor: 'transparent',
        backgroundColor: colour + '22',
        borderWidth: 0, pointRadius: 0, tension: 0.15, spanGaps: false,
        fill: '+1', // fill between this dataset and the next (max)
        _isBand: true,
      });
      const dMaxIdx = state.dopplerChart.data.datasets.length;
      state.dopplerChart.data.datasets.push({
        label: label + ' max',
        data: dopplerMaxPoints,
        borderColor: 'transparent',
        backgroundColor: colour + '22',
        borderWidth: 0, pointRadius: 0, tension: 0.15, spanGaps: false,
        fill: false,
        _isBand: true,
      });
      const dIdx = state.dopplerChart.data.datasets.length;
      state.dopplerChart.data.datasets.push({
        label, data: dopplerPoints,
        borderColor: colour, backgroundColor: 'transparent',
        borderWidth: 1.5, pointRadius: 0, tension: 0.15, spanGaps: false,
      });

      const sIdx = state.snrChart.data.datasets.length;
      state.snrChart.data.datasets.push({
        label, data: snrPoints,
        borderColor: colour, backgroundColor: colour + '22', fill: true,
        borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
      });

      const pIdx = state.powerChart.data.datasets.length;
      state.powerChart.data.datasets.push({
        label, data: powerPoints,
        borderColor: colour, backgroundColor: colour + '22', fill: true,
        borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
      });

      state.chartDatasets[label] = { doppler: dIdx, dMin: dMinIdx, dMax: dMaxIdx, snr: sIdx, power: pIdx };
    } catch (e) {
      console.warn('history load failed for', label, e);
    }
  }

  updateDopplerChartAxis();
  state.dopplerChart.update('none');
  state.snrChart.update('none');
  state.powerChart.update('none');
}

function appendLivePoint(label, reading) {
  const i = state.stations.findIndex(s => s.config && s.config.label === label);
  const s = i >= 0 ? state.stations[i] : null;
  // Skip reference station on charts unless user opted in
  if (s && s.config.is_reference && !state.showRef) return;
  const nominalHz = s ? s.config.freq_hz : 0;
  const colour = colourForIndex(i >= 0 ? i : state.dopplerChart.data.datasets.length);
  const cutoff = Date.now() - state.historyHours * 3600 * 1000;
  const ts = new Date(reading.timestamp);

  if (state.chartDatasets[label] === undefined) {
    // Create min band, max band, then mean line (same order as loadHistory)
    const dMinIdx = state.dopplerChart.data.datasets.length;
    state.dopplerChart.data.datasets.push({
      label: label + ' min', data: [], borderColor: 'transparent',
      backgroundColor: colour + '22', borderWidth: 0, pointRadius: 0,
      tension: 0.15, spanGaps: false, fill: '+1', _isBand: true,
    });
    const dMaxIdx = state.dopplerChart.data.datasets.length;
    state.dopplerChart.data.datasets.push({
      label: label + ' max', data: [], borderColor: 'transparent',
      backgroundColor: colour + '22', borderWidth: 0, pointRadius: 0,
      tension: 0.15, spanGaps: false, fill: false, _isBand: true,
    });
    const dIdx = state.dopplerChart.data.datasets.length;
    state.dopplerChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: 'transparent',
      borderWidth: 1.5, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    const sIdx = state.snrChart.data.datasets.length;
    state.snrChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: colour + '22',
      fill: true, borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    const pIdx = state.powerChart.data.datasets.length;
    state.powerChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: colour + '22',
      fill: true, borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.chartDatasets[label] = { doppler: dIdx, dMin: dMinIdx, dMax: dMaxIdx, snr: sIdx, power: pIdx };
  }

  const { doppler: dIdx, dMin: dMinIdx, dMax: dMaxIdx, snr: sIdx, power: pIdx } = state.chartDatasets[label];
  const dDs    = state.dopplerChart.data.datasets[dIdx];
  const dMinDs = dMinIdx !== undefined ? state.dopplerChart.data.datasets[dMinIdx] : null;
  const dMaxDs = dMaxIdx !== undefined ? state.dopplerChart.data.datasets[dMaxIdx] : null;
  const sDs    = state.snrChart.data.datasets[sIdx];
  const pDs    = state.powerChart.data.datasets[pIdx];

  // Use corrected Doppler when available (reference station offset applied)
  const dHz = (reading.corrected_doppler_hz !== null && reading.corrected_doppler_hz !== undefined)
    ? reading.corrected_doppler_hz : reading.doppler_hz;
  const yVal = reading.valid ? dopplerToY(dHz, nominalHz) : null;
  // Live 1-second readings don't have min/max/std — those are minute-mean stats
  dDs.data.push({ x: ts, y: yVal });
  if (dMinDs) dMinDs.data.push({ x: ts, y: yVal }); // live: band collapses to mean line
  if (dMaxDs) dMaxDs.data.push({ x: ts, y: yVal });
  sDs.data.push({ x: ts, y: reading.valid ? reading.snr_db : null });
  pDs.data.push({ x: ts, y: reading.valid ? reading.signal_dbfs : null });

  const trimDs = ds => {
    while (ds.data.length > 0 && ds.data[0].x.getTime() < cutoff) ds.data.shift();
  };
  trimDs(dDs);
  if (dMinDs) trimDs(dMinDs);
  if (dMaxDs) trimDs(dMaxDs);
  trimDs(sDs); trimDs(pDs);

  state.dopplerChart.update('none');
  state.snrChart.update('none');
  state.powerChart.update('none');
}

// ---------------------------------------------------------------------------
// SSE live feed
// ---------------------------------------------------------------------------
function connectSSE() {
  let retryDelay = 1000; // ms — doubles on each failure, capped at 30s
  const MAX_RETRY = 30000;

  function connect() {
    setConnStatus('connecting');
    const es = new EventSource(BASE + '/api/events');

    // Named events from the server — set connected on any of these
    es.addEventListener('connected',  () => { retryDelay = 1000; setConnStatus('connected'); });
    es.addEventListener('heartbeat',  () => { retryDelay = 1000; setConnStatus('connected'); });
    es.onopen = () => { retryDelay = 1000; setConnStatus('connected'); };

    // Fallback: if the proxy buffers the initial 'connected' event,
    // set connected after a short delay if the connection is open.
    setTimeout(() => {
      if (es.readyState === EventSource.OPEN) {
        retryDelay = 1000;
        setConnStatus('connected');
      }
    }, 3000);

    // Throttle spectrum refresh — at most once per 2 seconds
    let spectrumRefreshPending = false;
    const scheduleSpectrumRefresh = () => {
      if (spectrumRefreshPending) return;
      spectrumRefreshPending = true;
      setTimeout(async () => {
        spectrumRefreshPending = false;
        try {
          const r = await apiFetch('/api/stations');
          const data = await r.json() || [];
          // Merge spectrum_data, peak_bin and bin_bandwidth into existing station objects
          data.forEach(d => {
            const s = state.stations.find(x => x.config && x.config.label === d.config.label);
            if (s) {
              s.spectrum_data  = d.spectrum_data;
              s.peak_bin       = d.peak_bin;
              s.bin_bandwidth  = d.bin_bandwidth;
            }
          });
          drawMiniSpectra();
          updateSpectrumHints();
        } catch (e) {
          console.warn('spectrum refresh failed', e);
        }
      }, 2000);
    };

    es.onmessage = e => {
      retryDelay = 1000;
      setConnStatus('connected');
      try {
        const { station, reading } = JSON.parse(e.data);
        const s = state.stations.find(x => x.config && x.config.label === station);
        if (s) {
          s.current = reading;
          // Keep corrected_doppler_hz on the station object in sync with the live reading
          // so the status table column updates every second (not just on /api/stations polls)
          if (reading.corrected_doppler_hz !== undefined) {
            s.corrected_doppler_hz = reading.corrected_doppler_hz;
          }
          renderStatusTable();
          // Redraw mini spectrum with current reading info (uses cached spectrum_data)
          const i = state.stations.indexOf(s);
          const canvasId = `spec-canvas-${station.replace(/[^a-zA-Z0-9]/g, '_')}`;
          drawMiniSpectrum(canvasId, s, i);
        }
        appendLivePoint(station, reading);
        // Feed live reading into the audio analysis modal if it's open for this station
        if (typeof AudioAnalysisModal !== 'undefined' &&
            AudioAnalysisModal.isOpen() &&
            AudioAnalysisModal.activeLabel() === station) {
          AudioAnalysisModal.pushReading(reading);
        }
        // Periodically refresh spectrum data from server
        scheduleSpectrumRefresh();
      } catch (err) {
        console.warn('SSE parse error', err);
      }
    };

    es.onerror = () => {
      setConnStatus('disconnected');
      es.close();
      // Exponential backoff with ±20% jitter to avoid thundering herd
      const jitter = retryDelay * 0.2 * (Math.random() * 2 - 1);
      const delay = Math.min(MAX_RETRY, retryDelay + jitter);
      retryDelay = Math.min(MAX_RETRY, retryDelay * 2);
      setTimeout(connect, delay);
    };
  }

  connect();
}

// ---------------------------------------------------------------------------
// Station management UI
// ---------------------------------------------------------------------------
function renderStationList() {
  const el = document.getElementById('station-list');
  const canWrite = !state.auth.passwordConfigured || state.auth.authenticated;

  if (state.stations.length === 0) {
    el.innerHTML = '<p style="color:var(--muted)">No stations configured.</p>';
    return;
  }
  el.innerHTML = state.stations.map((s, i) => {
    const cfg = s.config;
    const colour = colourForIndex(i);
    const dis = cfg.enabled ? '' : ' station-disabled';
    const refNote = cfg.is_reference ? ' · <span style="color:var(--yellow)">reference</span>' : '';
    const actions = canWrite
      ? `<div class="station-actions">
          <button class="btn btn-secondary btn-sm" onclick="editStation('${cfg.id}')">Edit</button>
          <button class="btn btn-danger btn-sm" onclick="removeStation('${cfg.label}')">Remove</button>
        </div>`
      : '';
    return `<div class="station-card${dis}" data-id="${cfg.id}">
      <div class="station-info">
        <span class="station-name">
          <span class="station-dot" style="background:${colour}"></span>
          ${cfg.label}${cfg.is_reference ? ' <span class="ref-badge">REF</span>' : ''}
        </span>
        <span class="station-meta">${fmtHz(cfg.freq_hz)} · SNR ≥ ${cfg.min_snr} dB · ±${cfg.max_drift_hz} Hz · ${cfg.enabled ? 'enabled' : 'disabled'}${refNote}${cfg.callsign ? ' · ' + cfg.callsign : ''}${cfg.grid ? ' · ' + cfg.grid : ''}</span>
      </div>
      ${actions}
    </div>`;
  }).join('');
}

function populateDownloadSelect() {
  const sel = document.getElementById('dl-station');
  sel.innerHTML = state.stations.map(s =>
    `<option value="${s.config.label}">${s.config.label}</option>`
  ).join('');
}

function updateSpectrumHints() {
  // Use the first station with actual spectrum data to get real bin count and bandwidth
  const s = state.stations.find(x => x.spectrum_data && x.spectrum_data.length > 0 && x.bin_bandwidth > 0);
  if (!s) return;
  const bins = s.spectrum_data.length;
  const bw   = s.bin_bandwidth;
  const windowHz = (bins * bw).toFixed(0);
  const text = `${windowHz} Hz window (${bins} bins × ${bw} Hz/bin)`;
  const el1 = document.getElementById('hint-window-spec');
  const el2 = document.getElementById('hint-window-spec2');
  if (el1) el1.textContent = text;
  if (el2) el2.textContent = text;
}

async function loadStations() {
  const r = await apiFetch('/api/stations');
  state.stations = await r.json() || [];
  renderStatusTable();
  renderStationList();
  populateDownloadSelect();
  drawMiniSpectra();
  updateSpectrumHints();
}

// Modal helpers
function openModal(title, cfg = {}) {
  const isEdit = !!cfg.id;
  document.getElementById('modal-title').textContent = title;
  document.getElementById('f-id').value = cfg.id || '';
  document.getElementById('f-label').value = cfg.label || '';
  document.getElementById('f-freq').value = cfg.freq_hz || '';
  document.getElementById('f-callsign').value = cfg.callsign || '';
  document.getElementById('f-grid').value = cfg.grid || '';
  document.getElementById('f-min-snr').value = cfg.min_snr ?? 10;
  document.getElementById('f-max-drift').value = cfg.max_drift_hz ?? 50;
  document.getElementById('f-enabled').checked = cfg.enabled !== false;
  document.getElementById('f-reference').checked = cfg.is_reference === true;
  const presetRow = document.getElementById('preset-row');
  if (presetRow) presetRow.style.display = isEdit ? 'none' : '';
  const presetSel = document.getElementById('f-preset');
  if (presetSel) presetSel.value = '';
  document.getElementById('modal').classList.remove('hidden');
  document.getElementById('f-label').focus();
}

function closeModal() {
  document.getElementById('modal').classList.add('hidden');
}

window.editStation = function(id) {
  const s = state.stations.find(x => x.config && x.config.id === id);
  if (s) openModal('Edit Station', s.config);
};

window.removeStation = async function(label) {
  if (!confirm(`Remove station "${label}"?`)) return;
  try {
    await apiFetch('/api/stations/remove', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ label }),
    });
    await loadStations();
    await loadHistory();
  } catch (e) {
    alert('Error: ' + e.message);
  }
};

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------
document.addEventListener('DOMContentLoaded', async () => {
  initCharts();
  startSpecRenderLoop();
  await checkAuthStatus();
  await loadSettings();
  await loadStations();
  await loadHistory();
  connectSSE();

  // ── Auth buttons ─────────────────────────────────────────────────────────
  const authBtn = document.getElementById('auth-btn');
  if (authBtn) authBtn.addEventListener('click', openLoginModal);

  const logoutBtn = document.getElementById('logout-btn');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', async () => {
      try {
        await apiFetch('/api/auth/logout', { method: 'POST' });
        state.auth.authenticated = false;
        updateAuthUI();
        renderStationList();
      } catch (e) {
        console.warn('logout failed', e);
      }
    });
  }

  // ── Login modal ───────────────────────────────────────────────────────────
  const loginCancel = document.getElementById('login-cancel');
  if (loginCancel) loginCancel.addEventListener('click', closeLoginModal);
  const loginModal = document.getElementById('login-modal');
  if (loginModal) {
    loginModal.addEventListener('click', e => {
      if (e.target === loginModal) closeLoginModal();
    });
  }

  const loginForm = document.getElementById('login-form');
  if (loginForm) {
    loginForm.addEventListener('submit', async e => {
      e.preventDefault();
      const pw = document.getElementById('login-password').value;
      const errEl = document.getElementById('login-error');
      try {
        await apiFetch('/api/auth/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ password: pw }),
        });
        state.auth.authenticated = true;
        updateAuthUI();
        renderStationList();
        closeLoginModal();
      } catch (err) {
        errEl.textContent = 'Incorrect password.';
        errEl.style.display = '';
      }
    });
  }

  // ── Preset selector ──────────────────────────────────────────────────────
  const presetSel = document.getElementById('f-preset');
  if (presetSel) {
    presetSel.addEventListener('change', () => {
      const val = presetSel.value;
      if (!val) return;
      const [label, freq] = val.split(':');
      document.getElementById('f-label').value = label;
      document.getElementById('f-freq').value = freq;
      if (label.startsWith('REF-')) {
        document.getElementById('f-reference').checked = true;
      }
      presetSel.value = '';
    });
  }

  // ── Settings form ────────────────────────────────────────────────────────
  const settingsForm = document.getElementById('settings-form');
  if (settingsForm) {
    settingsForm.addEventListener('submit', async e => {
      e.preventDefault();
      const offsetEl = document.getElementById('s-manual-offset');
      const s = {
        callsign:              document.getElementById('s-callsign').value.trim(),
        grid:                  document.getElementById('s-grid').value.trim(),
        manual_offset_hz:      offsetEl ? parseFloat(offsetEl.value) || 0 : 0,
        frequency_reference:   document.getElementById('s-freq-ref').value,
        reference_description: document.getElementById('s-ref-desc').value.trim(),
      };
      try {
        await apiFetch('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(s),
        });
        updateRefBanner(s.frequency_reference);
        const saved = document.getElementById('settings-saved');
        if (saved) {
          saved.style.display = 'inline';
          setTimeout(() => { saved.style.display = 'none'; }, 3000);
        }
      } catch (err) {
        alert('Error saving settings: ' + err.message);
      }
    });
    document.getElementById('s-freq-ref').addEventListener('change', e => {
      updateRefBanner(e.target.value);
    });
  }

  // ── Hours selector ───────────────────────────────────────────────────────
  document.getElementById('hours-select').addEventListener('change', async e => {
    state.historyHours = parseInt(e.target.value, 10);
    // Clear date filter when rolling window is changed
    state.chartDate = '';
    const dateEl = document.getElementById('chart-date');
    if (dateEl) dateEl.value = '';
    const clearBtn = document.getElementById('chart-date-clear');
    if (clearBtn) clearBtn.style.display = 'none';
    await loadHistory();
  });

  // ── Date picker for historical chart view ────────────────────────────────
  const chartDateEl = document.getElementById('chart-date');
  const chartDateClearEl = document.getElementById('chart-date-clear');
  if (chartDateEl) {
    chartDateEl.addEventListener('change', async e => {
      state.chartDate = e.target.value; // YYYY-MM-DD or ''
      if (chartDateClearEl) chartDateClearEl.style.display = state.chartDate ? '' : 'none';
      await loadHistory();
    });
  }
  if (chartDateClearEl) {
    chartDateClearEl.addEventListener('click', async () => {
      state.chartDate = '';
      if (chartDateEl) chartDateEl.value = '';
      chartDateClearEl.style.display = 'none';
      await loadHistory();
    });
  }

  // ── Chart mode (doppler / absolute frequency) ────────────────────────────
  const chartModeEl = document.getElementById('chart-mode');
  if (chartModeEl) {
    chartModeEl.addEventListener('change', async e => {
      state.chartMode = e.target.value;
      await loadHistory();
    });
  }

  // ── SNR chart toggle ─────────────────────────────────────────────────────
  document.getElementById('show-snr-chart').addEventListener('change', e => {
    state.showSNR = e.target.checked;
    document.getElementById('snr-chart-wrap').style.display = state.showSNR ? '' : 'none';
  });

  // ── Power chart toggle ───────────────────────────────────────────────────
  const showPowerEl = document.getElementById('show-power-chart');
  if (showPowerEl) {
    showPowerEl.addEventListener('change', e => {
      state.showPower = e.target.checked;
      const wrap = document.getElementById('power-chart-wrap');
      if (wrap) wrap.style.display = state.showPower ? '' : 'none';
    });
  }

  // ── Reference station chart toggle ───────────────────────────────────────
  const showRefEl = document.getElementById('show-ref-station');
  if (showRefEl) {
    showRefEl.addEventListener('change', e => {
      state.showRef = e.target.checked;
      // Reload history charts to add/remove reference station dataset
      loadHistory();
    });
  }

  // ── Add button ───────────────────────────────────────────────────────────
  document.getElementById('add-btn').addEventListener('click', () => openModal('Add Station'));

  // ── Modal cancel ─────────────────────────────────────────────────────────
  document.getElementById('modal-cancel').addEventListener('click', closeModal);
  document.getElementById('modal').addEventListener('click', e => {
    if (e.target === document.getElementById('modal')) closeModal();
  });

  // ── Station form submit ───────────────────────────────────────────────────
  document.getElementById('station-form').addEventListener('submit', async e => {
    e.preventDefault();
    const id = document.getElementById('f-id').value;
    const cfg = {
      id,
      label:        document.getElementById('f-label').value.trim(),
      freq_hz:      parseInt(document.getElementById('f-freq').value, 10),
      callsign:     document.getElementById('f-callsign').value.trim(),
      grid:         document.getElementById('f-grid').value.trim(),
      min_snr:      parseFloat(document.getElementById('f-min-snr').value),
      max_drift_hz: parseFloat(document.getElementById('f-max-drift').value),
      enabled:      document.getElementById('f-enabled').checked,
      is_reference: document.getElementById('f-reference').checked,
    };
    try {
      const endpoint = id ? '/api/stations/update' : '/api/stations/add';
      await apiFetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(cfg),
      });
      closeModal();
      await loadStations();
      await loadHistory();
    } catch (err) {
      alert('Error: ' + err.message);
    }
  });

  // ── CSV download ──────────────────────────────────────────────────────────
  document.getElementById('dl-btn').addEventListener('click', () => {
    const station = document.getElementById('dl-station').value;
    const date    = document.getElementById('dl-date').value;
    if (!station || !date) { alert('Select a station and date.'); return; }
    window.location.href = `${BASE}/api/csv?station=${encodeURIComponent(station)}&date=${date}`;
  });

  // Default download date to today (UTC)
  document.getElementById('dl-date').value = new Date().toISOString().slice(0, 10);
});
