/* app.js — ubersdr_doppler web UI */
'use strict';

// ---------------------------------------------------------------------------
// Global settings
// ---------------------------------------------------------------------------
async function loadSettings() {
  try {
    const r = await apiFetch('/api/settings');
    const s = await r.json();
    document.getElementById('s-callsign').value = s.callsign || '';
    document.getElementById('s-grid').value = s.grid || '';
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
// State
// ---------------------------------------------------------------------------
const state = {
  stations: [],          // [{config, current}]
  dopplerChart: null,
  snrChart: null,
  chartDatasets: {},     // label → {doppler: idx, snr: idx}
  historyHours: 24,
  showSNR: true,
};

// Colour palette — one colour per station, shared across both charts
const COLOURS = [
  '#58a6ff', '#3fb950', '#f78166', '#d29922',
  '#bc8cff', '#39d353', '#ff7b72', '#ffa657',
];

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

// Format a UTC timestamp as HH:MM:SS UTC
function fmtUTC(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return d.toISOString().slice(11, 19) + ' UTC';
}

function colourForIndex(i) {
  return COLOURS[i % COLOURS.length];
}

function stationIndex(label) {
  return state.stations.findIndex(s => s.config.label === label);
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
// Check if any station is marked as reference
function hasReference() {
  return state.stations.some(s => s.config.is_reference);
}

function renderStatusTable() {
  const tbody = document.getElementById('status-tbody');
  const thead = document.querySelector('#status-table thead tr');
  const showRef = hasReference();

  // Update header to show/hide corrected column
  if (thead) {
    thead.innerHTML = `
      <th>Station</th>
      <th>Frequency</th>
      <th>Doppler (raw)</th>
      ${showRef ? '<th>Doppler (corrected)</th>' : ''}
      <th>SNR</th>
      <th>Signal</th>
      <th>Noise Floor</th>
      <th>Updated (UTC)</th>
      <th>State</th>`;
  }

  if (state.stations.length === 0) {
    const cols = showRef ? 9 : 8;
    tbody.innerHTML = `<tr><td colspan="${cols}" class="loading">No stations configured — add one below.</td></tr>`;
    return;
  }

  tbody.innerHTML = state.stations.map((s, i) => {
    const r = s.current || {};
    const valid = r.valid;
    const colour = colourForIndex(i);
    const isRef = s.config.is_reference;

    const dHz   = valid ? fmtDoppler(r.doppler_hz) : '—';
    const cls   = valid ? dopplerClass(r.doppler_hz) : 'invalid';
    const snr   = valid ? r.snr_db.toFixed(1) + ' dB' : '—';
    const sig   = valid ? r.signal_dbfs.toFixed(1) + ' dBFS' : '—';
    const noise = valid ? r.noise_dbfs.toFixed(1) + ' dBFS' : '—';
    const ts    = r.timestamp ? fmtUTC(r.timestamp) : '—';

    let corrCell = '';
    if (showRef) {
      if (isRef) {
        corrCell = '<td class="muted">— (reference)</td>';
      } else if (s.corrected_doppler_hz !== null && s.corrected_doppler_hz !== undefined) {
        const cHz = s.corrected_doppler_hz;
        corrCell = `<td class="${dopplerClass(cHz)}">${fmtDoppler(cHz)}</td>`;
      } else {
        corrCell = '<td class="invalid">—</td>';
      }
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
      <td>${snr}</td>
      <td>${sig}</td>
      <td>${noise}</td>
      <td>${ts}</td>
      <td>${stateTxt}</td>
    </tr>`;
  }).join('');
}

// ---------------------------------------------------------------------------
// Charts
// ---------------------------------------------------------------------------

// Shared x-axis config for both charts
function xAxisConfig() {
  return {
    type: 'time',
    time: {
      unit: 'minute',
      displayFormats: { minute: 'HH:mm', hour: 'HH:mm' },
      tooltipFormat: 'HH:mm:ss',
    },
    ticks: { color: '#8b949e', maxTicksLimit: 10, source: 'auto' },
    grid: { color: '#21262d' },
    title: { display: true, text: 'UTC time', color: '#8b949e', font: { size: 11 } },
  };
}

function initCharts() {
  // ── Doppler chart ──────────────────────────────────────────────────────
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
        x: xAxisConfig(),
        y: {
          title: { display: true, text: 'Doppler shift (Hz)', color: '#8b949e', font: { size: 11 } },
          ticks: { color: '#8b949e' },
          grid: { color: '#21262d' },
          // Zero reference line
          afterDataLimits(scale) {
            // Ensure zero is always visible
            if (scale.max < 0.5) scale.max = 0.5;
            if (scale.min > -0.5) scale.min = -0.5;
          },
        },
      },
      plugins: {
        legend: { labels: { color: '#e6edf3', boxWidth: 12 } },
        tooltip: {
          callbacks: {
            title: items => {
              if (!items.length) return '';
              return new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC';
            },
            label: ctx => `${ctx.dataset.label}: ${fmtDoppler(ctx.parsed.y)}`,
          },
        },
        // Zero reference line annotation (drawn manually via afterDraw plugin)
      },
    },
    plugins: [{
      id: 'zeroLine',
      afterDraw(chart) {
        const yScale = chart.scales.y;
        const xScale = chart.scales.x;
        if (!yScale || !xScale) return;
        const y = yScale.getPixelForValue(0);
        const ctx = chart.ctx;
        ctx.save();
        ctx.beginPath();
        ctx.moveTo(xScale.left, y);
        ctx.lineTo(xScale.right, y);
        ctx.strokeStyle = 'rgba(255,255,255,0.25)';
        ctx.lineWidth = 1;
        ctx.setLineDash([4, 4]);
        ctx.stroke();
        ctx.restore();
      },
    }],
  });

  // ── SNR chart ──────────────────────────────────────────────────────────
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
        x: { ...xAxisConfig(), title: { display: false } },
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
            title: items => {
              if (!items.length) return '';
              return new Date(items[0].parsed.x).toISOString().slice(11, 19) + ' UTC';
            },
            label: ctx => `${ctx.dataset.label} SNR: ${ctx.parsed.y.toFixed(1)} dB`,
          },
        },
      },
    },
  });
}

async function loadHistory() {
  const cutoff = Date.now() - state.historyHours * 3600 * 1000;
  state.dopplerChart.data.datasets = [];
  state.snrChart.data.datasets = [];
  state.chartDatasets = {};

  for (let i = 0; i < state.stations.length; i++) {
    const label = state.stations[i].config.label;
    const colour = colourForIndex(i);
    try {
      const r = await apiFetch(`/api/history?station=${encodeURIComponent(label)}`);
      const history = await r.json() || [];
      const filtered = history.filter(m => new Date(m.timestamp).getTime() >= cutoff);

      const dopplerPoints = filtered.map(m => ({ x: new Date(m.timestamp), y: m.doppler_hz }));
      const snrPoints     = filtered.map(m => ({ x: new Date(m.timestamp), y: m.snr_db }));

      const dIdx = state.dopplerChart.data.datasets.length;
      state.dopplerChart.data.datasets.push({
        label,
        data: dopplerPoints,
        borderColor: colour,
        backgroundColor: 'transparent',
        borderWidth: 1.5,
        pointRadius: 0,
        tension: 0.15,
        spanGaps: false, // gaps where no valid reading = visible break
      });

      const sIdx = state.snrChart.data.datasets.length;
      state.snrChart.data.datasets.push({
        label,
        data: snrPoints,
        borderColor: colour,
        backgroundColor: colour + '22',
        fill: true,
        borderWidth: 1,
        pointRadius: 0,
        tension: 0.15,
        spanGaps: false,
      });

      state.chartDatasets[label] = { doppler: dIdx, snr: sIdx };
    } catch (e) {
      console.warn('history load failed for', label, e);
    }
  }

  state.dopplerChart.update('none');
  state.snrChart.update('none');
}

// Append a live 1-second reading to both charts.
function appendLivePoint(label, reading) {
  const i = stationIndex(label);
  const colour = colourForIndex(i >= 0 ? i : state.dopplerChart.data.datasets.length);
  const cutoff = Date.now() - state.historyHours * 3600 * 1000;
  const ts = new Date(reading.timestamp);

  // Ensure datasets exist
  if (state.chartDatasets[label] === undefined) {
    const dIdx = state.dopplerChart.data.datasets.length;
    const sIdx = state.snrChart.data.datasets.length;
    state.dopplerChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: 'transparent',
      borderWidth: 1.5, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.snrChart.data.datasets.push({
      label, data: [], borderColor: colour, backgroundColor: colour + '22',
      fill: true, borderWidth: 1, pointRadius: 0, tension: 0.15, spanGaps: false,
    });
    state.chartDatasets[label] = { doppler: dIdx, snr: sIdx };
  }

  const { doppler: dIdx, snr: sIdx } = state.chartDatasets[label];
  const dDs = state.dopplerChart.data.datasets[dIdx];
  const sDs = state.snrChart.data.datasets[sIdx];

  if (reading.valid) {
    dDs.data.push({ x: ts, y: reading.doppler_hz });
    sDs.data.push({ x: ts, y: reading.snr_db });
  }
  // Always push a null point when invalid so spanGaps=false creates a visible gap
  // (Chart.js skips null values, creating a break in the line)
  else {
    dDs.data.push({ x: ts, y: null });
    sDs.data.push({ x: ts, y: null });
  }

  // Trim old points
  const trimDs = ds => {
    while (ds.data.length > 0 && ds.data[0].x.getTime() < cutoff) ds.data.shift();
  };
  trimDs(dDs);
  trimDs(sDs);

  state.dopplerChart.update('none');
  state.snrChart.update('none');
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
      const s = state.stations.find(x => x.config.label === station);
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
    return `<div class="station-card${dis}" data-id="${cfg.id}">
      <div class="station-info">
        <span class="station-name">
          <span class="station-dot" style="background:${colour}"></span>
          ${cfg.label}
        </span>
        <span class="station-meta">${fmtHz(cfg.freq_hz)} · SNR ≥ ${cfg.min_snr} dB · ±${cfg.max_drift_hz} Hz · ${cfg.enabled ? 'enabled' : 'disabled'}${cfg.callsign ? ' · ' + cfg.callsign : ''}${cfg.grid ? ' · ' + cfg.grid : ''}</span>
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
  // Show preset row only when adding a new station
  const presetRow = document.getElementById('preset-row');
  if (presetRow) presetRow.style.display = isEdit ? 'none' : '';
  // Reset preset selector
  const presetSel = document.getElementById('f-preset');
  if (presetSel) presetSel.value = '';
  document.getElementById('modal').classList.remove('hidden');
  document.getElementById('f-label').focus();
}

function closeModal() {
  document.getElementById('modal').classList.add('hidden');
}

window.editStation = function(id) {
  const s = state.stations.find(x => x.config.id === id);
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
      const s = {
        callsign:              document.getElementById('s-callsign').value.trim(),
        grid:                  document.getElementById('s-grid').value.trim(),
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

  // ── Modal: hide preset row when editing ──────────────────────────────────
  // (openModal shows/hides preset-row based on whether id is set)

  // Hours selector
  document.getElementById('hours-select').addEventListener('change', async e => {
    state.historyHours = parseInt(e.target.value, 10);
    await loadHistory();
  });

  // SNR chart toggle
  document.getElementById('show-snr-chart').addEventListener('change', e => {
    state.showSNR = e.target.checked;
    const wrap = document.getElementById('snr-chart-wrap');
    wrap.style.display = state.showSNR ? '' : 'none';
  });

  // Add button
  document.getElementById('add-btn').addEventListener('click', () => openModal('Add Station'));

  // Modal cancel
  document.getElementById('modal-cancel').addEventListener('click', closeModal);
  document.getElementById('modal').addEventListener('click', e => {
    if (e.target === document.getElementById('modal')) closeModal();
  });

  // Station form submit
  document.getElementById('station-form').addEventListener('submit', async e => {
    e.preventDefault();
    const id = document.getElementById('f-id').value;
    const cfg = {
      id,
      label:       document.getElementById('f-label').value.trim(),
      freq_hz:     parseInt(document.getElementById('f-freq').value, 10),
      callsign:    document.getElementById('f-callsign').value.trim(),
      grid:        document.getElementById('f-grid').value.trim(),
      min_snr:     parseFloat(document.getElementById('f-min-snr').value),
      max_drift_hz: parseFloat(document.getElementById('f-max-drift').value),
      enabled:     document.getElementById('f-enabled').checked,
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

  // CSV download
  document.getElementById('dl-btn').addEventListener('click', () => {
    const station = document.getElementById('dl-station').value;
    const date    = document.getElementById('dl-date').value;
    if (!station || !date) { alert('Select a station and date.'); return; }
    window.location.href = `/api/csv?station=${encodeURIComponent(station)}&date=${date}`;
  });

  // Default download date to today (UTC)
  document.getElementById('dl-date').value = new Date().toISOString().slice(0, 10);
});
