/* audio-analysis.js — Live audio analysis modal for ubersdr_doppler
 *
 * Opens when the user clicks "Listen" on a station row.
 * Streams the WAV preview, decodes PCM in JS, runs a real-time FFT,
 * and draws:
 *   1. FFT spectrum with real-world frequency axis
 *      (dial_freq_hz + bin_hz, so the carrier appears at carrier_freq_hz)
 *   2. 60-second rolling history of signal power (dBFS) and SNR (dB)
 *      sourced from the SSE live feed already running in app.js
 */
'use strict';

// ---------------------------------------------------------------------------
// Cooley-Tukey radix-2 FFT (in-place, power-of-2 size)
// Returns magnitude spectrum (dBFS) for the first N/2 bins.
// ---------------------------------------------------------------------------
function fftMagnitudeDB(samples) {
  const N = samples.length; // must be power of 2
  // Copy into complex arrays
  const re = new Float32Array(N);
  const im = new Float32Array(N);
  for (let i = 0; i < N; i++) re[i] = samples[i];

  // Bit-reversal permutation
  let j = 0;
  for (let i = 1; i < N; i++) {
    let bit = N >> 1;
    for (; j & bit; bit >>= 1) j ^= bit;
    j ^= bit;
    if (i < j) {
      [re[i], re[j]] = [re[j], re[i]];
      [im[i], im[j]] = [im[j], im[i]];
    }
  }

  // FFT butterfly
  for (let len = 2; len <= N; len <<= 1) {
    const ang = -2 * Math.PI / len;
    const wRe = Math.cos(ang);
    const wIm = Math.sin(ang);
    for (let i = 0; i < N; i += len) {
      let curRe = 1, curIm = 0;
      for (let k = 0; k < len / 2; k++) {
        const uRe = re[i + k];
        const uIm = im[i + k];
        const vRe = re[i + k + len / 2] * curRe - im[i + k + len / 2] * curIm;
        const vIm = re[i + k + len / 2] * curIm + im[i + k + len / 2] * curRe;
        re[i + k]           = uRe + vRe;
        im[i + k]           = uIm + vIm;
        re[i + k + len / 2] = uRe - vRe;
        im[i + k + len / 2] = uIm - vIm;
        const newRe = curRe * wRe - curIm * wIm;
        curIm = curRe * wIm + curIm * wRe;
        curRe = newRe;
      }
    }
  }

  // Magnitude in dBFS (normalised to full-scale 16-bit = 32768)
  const half = N / 2;
  const mag = new Float32Array(half);
  const scale = 1 / (N * 32768);
  for (let i = 0; i < half; i++) {
    const m = Math.sqrt(re[i] * re[i] + im[i] * im[i]) * scale;
    mag[i] = m > 1e-10 ? 20 * Math.log10(m) : -120;
  }
  return mag;
}

// Next power of 2 ≥ n
function nextPow2(n) {
  let p = 1;
  while (p < n) p <<= 1;
  return p;
}

// ---------------------------------------------------------------------------
// Hann window coefficients
// ---------------------------------------------------------------------------
function hannWindow(N) {
  const w = new Float32Array(N);
  for (let i = 0; i < N; i++) w[i] = 0.5 * (1 - Math.cos(2 * Math.PI * i / (N - 1)));
  return w;
}

// ---------------------------------------------------------------------------
// AudioAnalysisModal — self-contained controller
// ---------------------------------------------------------------------------
const AudioAnalysisModal = (() => {
  // State
  let _label = null;
  let _dialFreqHz = 0;
  let _carrierFreqHz = 0;
  let _sampleRate = 12000;
  let _abortController = null;
  let _animFrame = null;
  let _open = false;
  let _audioEl = null; // <audio> element for actual playback

  // Ring buffer for PCM samples (Float32, normalised -1..1)
  const RING_SIZE = 65536; // must be power of 2
  const ring = new Float32Array(RING_SIZE);
  let ringHead = 0; // next write position

  // FFT config
  const FFT_SIZE = 4096; // bins used for spectrum
  let hannWin = null;

  // History: 60 seconds × ~10 updates/sec = 600 points max
  const HISTORY_SECS = 60;
  const histSignal = []; // {t, v} dBFS
  const histSNR    = []; // {t, v} dB

  // Canvas refs
  let fftCanvas = null;
  let histCanvas = null;

  // Smoothed FFT magnitude (exponential moving average)
  let smoothMag = null;
  const SMOOTH_ALPHA = 0.25; // blend factor for new frame

  // ---------------------------------------------------------------------------
  // Public: register a new signal/SNR reading from the SSE feed
  // Called by app.js whenever a reading arrives for the active station.
  // ---------------------------------------------------------------------------
  function pushReading(reading) {
    if (!_open || !reading.valid) return;
    const now = Date.now();
    const cutoff = now - HISTORY_SECS * 1000;
    histSignal.push({ t: now, v: reading.signal_dbfs });
    histSNR.push({ t: now, v: reading.snr_db });
    // Trim old points
    while (histSignal.length > 0 && histSignal[0].t < cutoff) histSignal.shift();
    while (histSNR.length > 0 && histSNR[0].t < cutoff) histSNR.shift();
  }

  // ---------------------------------------------------------------------------
  // Open the modal for a given station
  // ---------------------------------------------------------------------------
  async function open(label) {
    if (_open) close();
    _label = label;
    _open = true;

    // Clear history
    histSignal.length = 0;
    histSNR.length = 0;
    smoothMag = null;
    ringHead = 0;
    ring.fill(0);

    // Show modal
    const modal = document.getElementById('audio-modal');
    modal.classList.remove('hidden');
    document.getElementById('audio-modal-title').textContent = `🎧 ${label} — Audio Analysis`;
    document.getElementById('audio-modal-subtitle').textContent = 'Fetching stream info…';
    setStatus('connecting', '⬤ Connecting…');
    document.getElementById('audio-fft-info').textContent = '';
    document.getElementById('fft-carrier-label').textContent = '—';

    fftCanvas  = document.getElementById('audio-fft-canvas');
    histCanvas = document.getElementById('audio-history-canvas');

    // Fetch audio info (sample rate, dial freq)
    try {
      const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
      const r = await fetch(`${BASE}/api/audio/info?station=${encodeURIComponent(label)}`);
      if (!r.ok) throw new Error(await r.text());
      const info = await r.json();
      _sampleRate    = info.sample_rate    || 12000;
      _dialFreqHz    = info.dial_freq_hz   || 0;
      _carrierFreqHz = info.carrier_freq_hz || 0;

      document.getElementById('audio-modal-subtitle').textContent =
        `${fmtHzLocal(_carrierFreqHz)} carrier · ${_sampleRate} Hz sample rate · dial ${fmtHzLocal(_dialFreqHz)} (USB)`;
      document.getElementById('fft-carrier-label').textContent = fmtHzLocal(_carrierFreqHz);

      // FFT bin resolution
      const binHz = _sampleRate / FFT_SIZE;
      document.getElementById('audio-fft-info').textContent =
        `FFT: ${FFT_SIZE} pts · ${binHz.toFixed(2)} Hz/bin · ${(_sampleRate / 2).toFixed(0)} Hz span`;

      hannWin = hannWindow(FFT_SIZE);
    } catch (e) {
      setStatus('error', `⬤ Error: ${e.message}`);
      return;
    }

    // Start render loop
    _animFrame = requestAnimationFrame(renderLoop);

    // Start audio playback via a separate <audio> element (WAV stream)
    const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
    const audioUrl = `${BASE}/api/audio/preview?station=${encodeURIComponent(label)}`;
    _audioEl = new Audio(audioUrl);
    _audioEl.play().catch(e => console.warn('audio playback failed:', e));
    _audioEl.onended = () => { if (_open) close(); };
    _audioEl.onerror = () => { if (_open) setStatus('error', '⬤ Playback error'); };

    // Start streaming audio for FFT analysis (separate fetch connection)
    _abortController = new AbortController();
    streamAudio(_abortController.signal);
  }

  // ---------------------------------------------------------------------------
  // Close the modal — stops both playback and the FFT analysis stream
  // ---------------------------------------------------------------------------
  function close() {
    _open = false;
    _label = null;
    // Stop audio playback
    if (_audioEl) {
      _audioEl.onended = null;
      _audioEl.onerror = null;
      _audioEl.pause();
      _audioEl.src = '';
      _audioEl.load(); // abort the HTTP stream
      _audioEl = null;
    }
    // Abort the FFT fetch stream
    if (_abortController) { _abortController.abort(); _abortController = null; }
    if (_animFrame) { cancelAnimationFrame(_animFrame); _animFrame = null; }
    const modal = document.getElementById('audio-modal');
    if (modal) modal.classList.add('hidden');
    // Sync app.js state so the Listen button reverts to its default label
    if (typeof state !== 'undefined') {
      state.audioPlaying = null;
      if (typeof renderStatusTable === 'function') renderStatusTable();
    }
  }

  // ---------------------------------------------------------------------------
  // Stream WAV audio, decode PCM, fill ring buffer
  // ---------------------------------------------------------------------------
  async function streamAudio(signal) {
    const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
    const url = `${BASE}/api/audio/preview?station=${encodeURIComponent(_label)}`;

    try {
      const resp = await fetch(url, { signal });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);

      const reader = resp.body.getReader();
      let wavHeaderSkipped = false;
      let headerBuf = new Uint8Array(0);

      setStatus('live', '⬤ Live');

      while (true) {
        const { done, value } = await reader.read();
        if (done || signal.aborted) break;

        let chunk = value;

        // Skip the 44-byte WAV header on the first chunk(s)
        if (!wavHeaderSkipped) {
          const combined = new Uint8Array(headerBuf.length + chunk.length);
          combined.set(headerBuf);
          combined.set(chunk, headerBuf.length);
          headerBuf = combined;
          if (headerBuf.length < 44) continue;
          // Verify RIFF magic
          const magic = String.fromCharCode(headerBuf[0], headerBuf[1], headerBuf[2], headerBuf[3]);
          if (magic !== 'RIFF') {
            // Not a WAV header — treat all bytes as raw PCM
            wavHeaderSkipped = true;
            chunk = headerBuf;
            headerBuf = new Uint8Array(0);
          } else {
            wavHeaderSkipped = true;
            chunk = headerBuf.slice(44);
            headerBuf = new Uint8Array(0);
          }
        }

        // Decode 16-bit little-endian PCM samples
        if (chunk.length < 2) continue;
        const nSamples = Math.floor(chunk.length / 2);
        const view = new DataView(chunk.buffer, chunk.byteOffset, chunk.byteLength);
        for (let i = 0; i < nSamples; i++) {
          const s16 = view.getInt16(i * 2, true); // little-endian
          ring[ringHead & (RING_SIZE - 1)] = s16; // store raw int16 (FFT normalises)
          ringHead++;
        }
      }
    } catch (e) {
      if (!signal.aborted) {
        setStatus('error', `⬤ Stream error: ${e.message}`);
      }
    }
  }

  // ---------------------------------------------------------------------------
  // Render loop — runs at rAF rate, draws FFT and history
  // ---------------------------------------------------------------------------
  function renderLoop() {
    if (!_open) return;
    drawFFT();
    drawHistory();
    _animFrame = requestAnimationFrame(renderLoop);
  }

  // ---------------------------------------------------------------------------
  // Draw FFT spectrum canvas
  // ---------------------------------------------------------------------------
  function drawFFT() {
    if (!fftCanvas || !hannWin) return;
    const dpr = window.devicePixelRatio || 1;
    const cssW = fftCanvas.clientWidth || 760;
    const cssH = fftCanvas.clientHeight || 160;
    const W = Math.round(cssW * dpr);
    const H = Math.round(cssH * dpr);
    if (fftCanvas.width !== W || fftCanvas.height !== H) {
      fftCanvas.width = W; fftCanvas.height = H;
    }
    const ctx = fftCanvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const CW = cssW, CH = cssH;

    const ML = 46, MB = 20;
    const plotW = CW - ML, plotH = CH - MB;

    ctx.clearRect(0, 0, CW, CH);
    ctx.fillStyle = '#0d1117';
    ctx.fillRect(0, 0, CW, CH);

    // Extract FFT_SIZE samples from ring buffer (most recent)
    const available = Math.min(ringHead, RING_SIZE);
    if (available < FFT_SIZE) {
      ctx.fillStyle = '#8b949e';
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('Buffering audio…', ML + plotW / 2, CH / 2);
      return;
    }

    const samples = new Float32Array(FFT_SIZE);
    const startIdx = ringHead - FFT_SIZE;
    for (let i = 0; i < FFT_SIZE; i++) {
      samples[i] = ring[(startIdx + i) & (RING_SIZE - 1)] * hannWin[i];
    }

    const mag = fftMagnitudeDB(samples);
    const half = mag.length; // FFT_SIZE / 2

    // Smooth
    if (!smoothMag || smoothMag.length !== half) {
      smoothMag = new Float32Array(mag);
    } else {
      for (let i = 0; i < half; i++) {
        smoothMag[i] = smoothMag[i] * (1 - SMOOTH_ALPHA) + mag[i] * SMOOTH_ALPHA;
      }
    }

    // dB range
    let minDB = Infinity, maxDB = -Infinity;
    for (let i = 1; i < half; i++) { // skip DC bin
      if (smoothMag[i] < minDB) minDB = smoothMag[i];
      if (smoothMag[i] > maxDB) maxDB = smoothMag[i];
    }
    if (!isFinite(minDB)) { minDB = -120; maxDB = -40; }
    const dbFloor = Math.floor(minDB / 10) * 10;
    const dbCeil  = Math.ceil(maxDB / 10) * 10;
    const dbRange = dbCeil - dbFloor || 10;
    const dbToY = db => plotH - ((db - dbFloor) / dbRange) * plotH;

    // Draw spectrum bars
    ctx.fillStyle = '#58a6ff88';
    const barW = plotW / (half - 1);
    for (let i = 1; i < half; i++) {
      const x = ML + (i - 1) / (half - 1) * plotW;
      const y = dbToY(smoothMag[i]);
      ctx.fillRect(x, y, Math.max(1, barW), plotH - y);
    }

    // Y axis
    ctx.font = '9px sans-serif';
    ctx.textAlign = 'right';
    const dbStep = dbRange <= 20 ? 5 : dbRange <= 40 ? 10 : 20;
    for (let db = dbFloor; db <= dbCeil; db += dbStep) {
      const y = dbToY(db);
      if (y < 0 || y > plotH) continue;
      ctx.fillStyle = '#8b949e';
      ctx.fillText(db + ' dB', ML - 3, y + 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(ML, y); ctx.lineTo(CW, y); ctx.stroke();
      ctx.setLineDash([]);
    }

    // X axis — real-world frequency
    // bin i → freq = dialFreqHz + i * (sampleRate / FFT_SIZE)
    const binHz = _sampleRate / FFT_SIZE;
    const freqStart = _dialFreqHz;                        // bin 0 (DC)
    const freqEnd   = _dialFreqHz + (half - 1) * binHz;  // bin half-1
    const freqSpan  = freqEnd - freqStart;

    // Nice label step
    const rawStep = freqSpan / 6;
    const mag10 = Math.pow(10, Math.floor(Math.log10(Math.abs(rawStep) || 1)));
    const niceSteps = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000];
    let freqStep = (niceSteps.find(v => v * mag10 >= rawStep) || 1000) * mag10;
    if (freqStep < 1) freqStep = 1;

    ctx.font = '9px sans-serif';
    ctx.textAlign = 'center';
    const firstLabel = Math.ceil(freqStart / freqStep) * freqStep;
    for (let f = firstLabel; f <= freqEnd; f += freqStep) {
      const x = ML + ((f - freqStart) / freqSpan) * plotW;
      if (x < ML || x > CW) continue;
      ctx.fillStyle = '#8b949e';
      ctx.fillText(fmtHzLocal(f), x, CH - 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, plotH); ctx.stroke();
      ctx.setLineDash([]);
    }
    ctx.textAlign = 'right';
    ctx.fillStyle = '#555';
    ctx.fillText('Hz', CW - 2, CH - 3);

    // Carrier marker (green dashed) at carrierFreqHz
    if (_carrierFreqHz >= freqStart && _carrierFreqHz <= freqEnd) {
      const cx = ML + ((_carrierFreqHz - freqStart) / freqSpan) * plotW;
      ctx.strokeStyle = '#3fb950';
      ctx.lineWidth = 1.5;
      ctx.setLineDash([4, 3]);
      ctx.beginPath(); ctx.moveTo(cx, 0); ctx.lineTo(cx, plotH); ctx.stroke();
      ctx.setLineDash([]);
      // Label
      ctx.fillStyle = '#3fb950';
      ctx.font = '9px sans-serif';
      ctx.textAlign = cx > CW - 60 ? 'right' : 'left';
      ctx.fillText('carrier', cx + (ctx.textAlign === 'left' ? 3 : -3), 10);
    }

    // Axis border
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.strokeRect(ML, 0, plotW, plotH);
  }

  // ---------------------------------------------------------------------------
  // Draw 60-second history canvas (signal dBFS + SNR dB)
  // ---------------------------------------------------------------------------
  function drawHistory() {
    if (!histCanvas) return;
    const dpr = window.devicePixelRatio || 1;
    const cssW = histCanvas.clientWidth || 760;
    const cssH = histCanvas.clientHeight || 130;
    const W = Math.round(cssW * dpr);
    const H = Math.round(cssH * dpr);
    if (histCanvas.width !== W || histCanvas.height !== H) {
      histCanvas.width = W; histCanvas.height = H;
    }
    const ctx = histCanvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const CW = cssW, CH = cssH;

    const ML = 46, MB = 18;
    const plotW = CW - ML, plotH = CH - MB;

    ctx.clearRect(0, 0, CW, CH);
    ctx.fillStyle = '#0d1117';
    ctx.fillRect(0, 0, CW, CH);

    const now = Date.now();
    const tStart = now - HISTORY_SECS * 1000;
    const tToX = t => ML + ((t - tStart) / (HISTORY_SECS * 1000)) * plotW;

    if (histSignal.length < 2) {
      ctx.fillStyle = '#8b949e';
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('Waiting for signal data…', ML + plotW / 2, CH / 2);
      return;
    }

    // Compute ranges
    let sigMin = Infinity, sigMax = -Infinity;
    let snrMin = Infinity, snrMax = -Infinity;
    for (const p of histSignal) { if (p.v < sigMin) sigMin = p.v; if (p.v > sigMax) sigMax = p.v; }
    for (const p of histSNR)    { if (p.v < snrMin) snrMin = p.v; if (p.v > snrMax) snrMax = p.v; }
    if (!isFinite(sigMin)) { sigMin = -120; sigMax = -40; }
    if (!isFinite(snrMin)) { snrMin = 0; snrMax = 40; }

    // Split canvas vertically: top 60% = signal, bottom 40% = SNR
    const splitY = Math.round(plotH * 0.58);
    const sigH = splitY - 2;
    const snrH = plotH - splitY - 2;

    const sigFloor = Math.floor(sigMin / 5) * 5;
    const sigCeil  = Math.ceil(sigMax / 5) * 5;
    const sigRange = sigCeil - sigFloor || 10;
    const sigToY = v => sigH - ((v - sigFloor) / sigRange) * sigH;

    const snrFloor = Math.max(0, Math.floor(snrMin / 5) * 5);
    const snrCeil  = Math.ceil(snrMax / 5) * 5;
    const snrRange = snrCeil - snrFloor || 10;
    const snrToY = v => splitY + 4 + snrH - ((v - snrFloor) / snrRange) * snrH;

    // ── Signal power (dBFS) ──────────────────────────────────────────────
    // Y axis
    ctx.font = '9px sans-serif';
    ctx.textAlign = 'right';
    const sigStep = sigRange <= 20 ? 5 : 10;
    for (let v = sigFloor; v <= sigCeil; v += sigStep) {
      const y = sigToY(v);
      if (y < 0 || y > sigH) continue;
      ctx.fillStyle = '#8b949e';
      ctx.fillText(v, ML - 3, y + 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(ML, y); ctx.lineTo(CW, y); ctx.stroke();
      ctx.setLineDash([]);
    }
    // Label
    ctx.save();
    ctx.fillStyle = '#58a6ff';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'left';
    ctx.fillText('dBFS', ML + 2, 9);
    ctx.restore();

    // Line
    ctx.strokeStyle = '#58a6ff';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    let first = true;
    for (const p of histSignal) {
      const x = tToX(p.t);
      const y = sigToY(p.v);
      if (x < ML || x > CW) continue;
      if (first) { ctx.moveTo(x, y); first = false; } else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // Divider
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.beginPath();
    ctx.moveTo(ML, splitY);
    ctx.lineTo(CW, splitY);
    ctx.stroke();

    // ── SNR (dB) ─────────────────────────────────────────────────────────
    const snrStep = snrRange <= 20 ? 5 : 10;
    for (let v = snrFloor; v <= snrCeil; v += snrStep) {
      const y = snrToY(v);
      if (y < splitY + 4 || y > plotH) continue;
      ctx.fillStyle = '#8b949e';
      ctx.font = '9px sans-serif';
      ctx.textAlign = 'right';
      ctx.fillText(v, ML - 3, y + 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(ML, y); ctx.lineTo(CW, y); ctx.stroke();
      ctx.setLineDash([]);
    }
    // Label
    ctx.save();
    ctx.fillStyle = '#3fb950';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'left';
    ctx.fillText('SNR dB', ML + 2, splitY + 12);
    ctx.restore();

    // Line
    ctx.strokeStyle = '#3fb950';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    first = true;
    for (const p of histSNR) {
      const x = tToX(p.t);
      const y = snrToY(p.v);
      if (x < ML || x > CW) continue;
      if (first) { ctx.moveTo(x, y); first = false; } else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // ── X axis (time, seconds ago) ────────────────────────────────────────
    ctx.font = '9px sans-serif';
    ctx.textAlign = 'center';
    for (let s = 0; s <= HISTORY_SECS; s += 10) {
      const x = ML + (s / HISTORY_SECS) * plotW;
      const label = s === 0 ? '−60s' : s === HISTORY_SECS ? 'now' : `−${HISTORY_SECS - s}s`;
      ctx.fillStyle = '#8b949e';
      ctx.fillText(label, x, CH - 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, plotH); ctx.stroke();
      ctx.setLineDash([]);
    }

    // Axis border
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.strokeRect(ML, 0, plotW, plotH);
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------
  function setStatus(type, text) {
    const el = document.getElementById('audio-status-text');
    if (!el) return;
    el.className = 'audio-status-' + type;
    el.textContent = text;
  }

  function fmtHzLocal(hz) {
    if (Math.abs(hz) >= 1e6) return (hz / 1e6).toFixed(3) + ' MHz';
    if (Math.abs(hz) >= 1e3) return (hz / 1e3).toFixed(3) + ' kHz';
    return hz.toFixed(0) + ' Hz';
  }

  // ---------------------------------------------------------------------------
  // Public API
  // ---------------------------------------------------------------------------
  return { open, close, pushReading, isOpen: () => _open, activeLabel: () => _label };
})();

// ---------------------------------------------------------------------------
// Wire up modal close button and backdrop click once DOM is ready
// ---------------------------------------------------------------------------
document.addEventListener('DOMContentLoaded', () => {
  const closeBtn = document.getElementById('audio-modal-close');
  if (closeBtn) closeBtn.addEventListener('click', () => AudioAnalysisModal.close());

  const modal = document.getElementById('audio-modal');
  if (modal) {
    modal.addEventListener('click', e => {
      if (e.target === modal) AudioAnalysisModal.close();
    });
  }
});
