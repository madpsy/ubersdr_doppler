/* app.js — ubersdr_doppler web UI
 *
 * Three-panel chart layout matching HamSCI Grape paper style:
 *   Panel 1: Doppler shift (Hz offset) OR absolute received frequency (Hz)
 *   Panel 2: SNR (dB)
 *   Panel 3: Signal power (dBFS)
 */
'use strict';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
const state = {
  stations: [],          // [{config, current, baseline_mean_hz, corrected_doppler_hz}]
  dopplerChart: null,
  snrChart: null,
  powerChart: null,
  chartDatasets: {},     // label → {doppler: idx, snr: idx, power: idx}
  historyHours: 24,
  showSNR: true,
  showPower: true,
  chartMode: 'doppler',  // 'doppler' | 'absolute'
};

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
    const nodeEl = document.getElementById('s-node');
    if (nodeEl) nodeEl.value = s.node_number || '';
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
      el.textContent = '✓ GPSDO — GPS-disciplined oscillator. Absolute Doppler values are accurate to ~0.01 Hz. Data is suitable for HamSCI submission.';
      break;
    case 'reference_station':
      el.classList.add('banner-refstat');
      el.textContent = '⚡ Reference station correction active. Hardware clock drift is cancelled in real time using the reference station signal. Mark one station as "Reference station" below.';
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
  const r = await fetch(path, opts);
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
      <th>State</th>`;
  }

  if (state.stations.length === 0) {
    const cols = showRef ? 10 : 9;
    tbody.innerHTML = `<tr><td colspan="${cols}" class="loading">No stations configured — add one below.</td></tr>`;
    return;
  }

  tbody.innerHTML = state.stations.map((s, i) => {
    const r = s.current || {};
    const valid = r.valid;
    const colour = colourForIndex(i);
    const isRef = s.config && s.config.is_reference;

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

    return `<tr>
      <td><span class="station-dot" style="background:${colour}"></span><strong>${s.config.label}</strong>${refBadge}</td>
      <td>${fmtHz(s.config.freq_hz)}</td>
      <td class="${cls}">${dHz}</td>
      ${corrCell}
      ${baselineCell}
      <td>${snr}</td>
      <td>${sig}</td>
      <td>${noise}</td>
      <td>${ts}</td>
      <td>${stateTxt}</td>
    </tr>`;
  }).join('');
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
        legend: { labels: { color: '#e6edf3', boxWidth: 12 } },
        tooltip: {
          callbacks: {
            title: items => items.length ? new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC' : '',
            label: ctx => {
              if (state.chartMode === 'absolute') {
                return `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(3)} Hz`;
              }
              return `${ctx.dataset.label}: ${fmtDoppler(ctx.parsed.y)}`;
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
  if (state.chartMode === 'absolute') {
    yAxis.title.text = 'Received frequency (Hz)';
  } else {
    yAxis.title.text = 'Doppler shift (Hz)';
  }
  chart.update('none');
}

async function loadHistory() {
  const cutoff = Date.now() - state.historyHours * 3600 * 1000;
  state.dopplerChart.data.datasets = [];
  state.snrChart.data.datasets = [];
  state.powerChart.data.datasets = [];
  state.chartDatasets = {};

  for (let i = 0; i < state.stations.length; i++) {
    const s = state.stations[i];
    const label = s.config.label;
    const nominalHz = s.config.freq_hz;
    const colour = colourForIndex(i);
    try {
      const r = await apiFetch(`/api/history?station=${encodeURIComponent(label)}`);
      const history = await r.json() || [];
      const filtered = history.filter(m => new Date(m.timestamp).getTime() >= cutoff);

      const dopplerPoints = filtered.map(m => ({ x: new Date(m.timestamp), y: dopplerToY(m.doppler_hz, nominalHz) }));
      const snrPoints     = filtered.map(m => ({ x: new Date(m.timestamp), y: m.snr_db }));
      const powerPoints   = filtered.map(m => ({ x: new Date(m.timestamp), y: m.signal_dbfs }));

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

      state.chartDatasets[label] = { doppler: dIdx, snr: sIdx, power: pIdx };
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
  const nominalHz = s ? s.config.freq_hz : 0;
  const colour = colourForIndex(i >= 0 ? i : state.dopplerChart.data.datasets.length);
  const cutoff = Date.now() - state.historyHours * 3600 * 1000;
  const ts = new Date(reading.timestamp);

  if (state.chartDatasets[label] === undefined) {
    const dIdx = state.dopplerChart.data.datasets.length;
    const sIdx = state.snrChart.data.datasets.length;
    const pIdx = state.powerChart.data.datasets.length;
    state.dopplerChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: 'transparent',
      borderWidth: 1.5, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.snrChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: colour + '22',
      fill: true, borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.powerChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: colour + '22',
      fill: true, borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.chartDatasets[label] = { doppler: dIdx, snr: sIdx, power: pIdx };
  }

  const { doppler: dIdx, snr: sIdx, power: pIdx } = state.chartDatasets[label];
  const dDs = state.dopplerChart.data.datasets[dIdx];
  const sDs = state.snrChart.data.datasets[sIdx];
  const pDs = state.powerChart.data.datasets[pIdx];

  const yVal = reading.valid ? dopplerToY(reading.doppler_hz, nominalHz) : null;
  dDs.data.push({ x: ts, y: yVal });
  sDs.data.push({ x: ts, y: reading.valid ? reading.snr_db : null });
  pDs.data.push({ x: ts, y: reading.valid ? reading.signal_dbfs : null });

  const trimDs = ds => {
    while (ds.data.length > 0 && ds.data[0].x.getTime() < cutoff) ds.data.shift();
  };
  trimDs(dDs); trimDs(sDs); trimDs(pDs);

  state.dopplerChart.update('none');
  state.snrChart.update('none');
  state.powerChart.update('none');
}

// ---------------------------------------------------------------------------
// SSE live feed
// ---------------------------------------------------------------------------
function connectSSE() {
  setConnStatus('connecting');
  const es = new EventSource('/api/events');

  es.onopen = () => setConnStatus('connected');

  es.onmessage = e => {
    try {
      const { station, reading } = JSON.parse(e.data);
      const s = state.stations.find(x => x.config && x.config.label === station);
      if (s) {
        s.current = reading;
        renderStatusTable();
      }
      appendLivePoint(station, reading);
    } catch (err) {
      console.warn('SSE parse error', err);
    }
  };

  es.onerror = () => {
    setConnStatus('disconnected');
    es.close();
    setTimeout(connectSSE, 5000);
  };
}

// ---------------------------------------------------------------------------
// Station management UI
// ---------------------------------------------------------------------------
function renderStationList() {
  const el = document.getElementById('station-list');
  if (state.stations.length === 0) {
    el.innerHTML = '<p style="color:var(--muted)">No stations configured.</p>';
    return;
  }
  el.innerHTML = state.stations.map((s, i) => {
    const cfg = s.config;
    const colour = colourForIndex(i);
    const dis = cfg.enabled ? '' : ' station-disabled';
    const refNote = cfg.is_reference ? ' · <span style="color:var(--yellow)">reference</span>' : '';
    return `<div class="station-card${dis}" data-id="${cfg.id}">
      <div class="station-info">
        <span class="station-name">
          <span class="station-dot" style="background:${colour}"></span>
          ${cfg.label}${cfg.is_reference ? ' <span class="ref-badge">REF</span>' : ''}
        </span>
        <span class="station-meta">${fmtHz(cfg.freq_hz)} · SNR ≥ ${cfg.min_snr} dB · ±${cfg.max_drift_hz} Hz · ${cfg.enabled ? 'enabled' : 'disabled'}${refNote}${cfg.callsign ? ' · ' + cfg.callsign : ''}${cfg.grid ? ' · ' + cfg.grid : ''}</span>
      </div>
      <div class="station-actions">
        <button class="btn btn-secondary btn-sm" onclick="editStation('${cfg.id}')">Edit</button>
        <button class="btn btn-danger btn-sm" onclick="removeStation('${cfg.label}')">Remove</button>
      </div>
    </div>`;
  }).join('');
}

function populateDownloadSelect() {
  const sel = document.getElementById('dl-station');
  sel.innerHTML = state.stations.map(s =>
    `<option value="${s.config.label}">${s.config.label}</option>`
  ).join('');
}

async function loadStations() {
  const r = await apiFetch('/api/stations');
  state.stations = await r.json() || [];
  renderStatusTable();
  renderStationList();
  populateDownloadSelect();
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
  document.getElementById('f-max-drift').value = cfg.max_drift_hz ?? 100;
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
  await loadSettings();
  await loadStations();
  await loadHistory();
  connectSSE();

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
      const nodeEl = document.getElementById('s-node');
      const offsetEl = document.getElementById('s-manual-offset');
      const s = {
        callsign:              document.getElementById('s-callsign').value.trim(),
        grid:                  document.getElementById('s-grid').value.trim(),
        node_number:           nodeEl ? nodeEl.value.trim() : '',
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
    await loadHistory();
  });

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
    window.location.href = `/api/csv?station=${encodeURIComponent(station)}&date=${date}`;
  });

  // Default download date to today (UTC)
  document.getElementById('dl-date').value = new Date().toISOString().slice(0, 10);
});
