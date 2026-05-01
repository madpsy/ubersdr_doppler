/* audio-analysis.js — Live audio analysis modal for ubersdr_doppler
 *
 * Opens when the user clicks "Listen" on a station row.
 * Uses a single <audio> element connected to the Web Audio API so that:
 *   • The browser plays the audio normally
 *   • An AnalyserNode provides real-time FFT data for the spectrum display
 *   • No duplicate HTTP connections are opened
 *
 * Draws:
 *   1. FFT spectrum with real-world frequency axis
 *      (dial_freq_hz + bin_hz, so the carrier appears at carrier_freq_hz)
 *   2. 60-second rolling history of signal power (dBFS) and SNR (dB)
 *      sourced from the SSE live feed already running in app.js
 */
'use strict';

// ---------------------------------------------------------------------------
// AudioAnalysisModal — self-contained controller
// ---------------------------------------------------------------------------
const AudioAnalysisModal = (() => {
  // State
  let _label          = null;
  let _dialFreqHz     = 0;
  let _carrierFreqHz  = 0;
  let _sampleRate     = 12000;
  let _open           = false;
  let _lastReading    = null;  // most recent SSE reading for the active station
  let _showExpected   = true;  // show green 'expected' carrier line
  let _showActual     = true;  // show red 'actual' detected frequency line

  // Audio passband limits (Hz, audio-relative from dial frequency).
  // Must match the bandwidthLow/bandwidthHigh sent to UberSDR in doppler.go.
  // The carrier tone appears at 1000 Hz in the audio passband (dial = carrier - 1000).
  const PASSBAND_LOW  = 300;   // Hz above dial frequency
  const PASSBAND_HIGH = 1500;  // Hz above dial frequency

  // Web Audio API objects
  let _audioCtx     = null;
  let _audioEl      = null;   // <audio> element — single connection for play + FFT
  let _analyser     = null;   // AnalyserNode
  let _sourceNode   = null;   // MediaElementSourceNode

  // rAF render loop handle
  let _animFrame    = null;

  // FFT config — AnalyserNode handles the FFT; we just read its output.
  // Web Audio API maximum is 32768 (2^15). At 12 kHz sample rate this gives
  // 16384 bins × (12000/32768) ≈ 0.37 Hz/bin — maximum possible resolution.
  const FFT_SIZE    = 32768;  // maximum allowed by Web Audio API spec

  // History: 60 seconds of signal/SNR readings from SSE
  const HISTORY_SECS = 60;
  const histSignal  = [];     // {t, v} dBFS
  const histSNR     = [];     // {t, v} dB

  // Canvas refs
  let fftCanvas     = null;
  let histCanvas    = null;

  // Smoothed FFT magnitude buffer (dB, from AnalyserNode.getFloatFrequencyData)
  let smoothMag     = null;
  const SMOOTH_ALPHA = 0.3;   // blend factor for new frame (higher = more responsive)

  // Smoothed Y-axis scale bounds — updated slowly so the axis doesn't jump every frame.
  // dbFloorSmooth tracks the noise floor (decays down slowly, snaps up quickly).
  // dbCeilSmooth  tracks the peak    (snaps up quickly, decays down slowly).
  let dbFloorSmooth = null;
  let dbCeilSmooth  = null;
  // How quickly the scale expands (fast) vs contracts (slow).
  const SCALE_EXPAND_ALPHA  = 0.15;  // fast: new peak appears → scale grows quickly
  const SCALE_SHRINK_ALPHA  = 0.005; // slow: peak gone → scale shrinks over ~200 frames

  // FFT zoom/pan state (in passband-bin space)
  // binView: { lo, hi } — the range of passband bins currently visible
  // null = full passband view (reset on open)
  let fftView = null; // { lo: number, hi: number }

  // ---------------------------------------------------------------------------
  // Public: register a new signal/SNR reading from the SSE feed
  // Called by app.js whenever a reading arrives for the active station.
  // ---------------------------------------------------------------------------
  function pushReading(reading) {
    if (!_open || !reading.valid) return;
    _lastReading = reading;
    const now    = Date.now();
    const cutoff = now - HISTORY_SECS * 1000;
    histSignal.push({ t: now, v: reading.signal_dbfs });
    histSNR.push(   { t: now, v: reading.snr_db });
    while (histSignal.length > 0 && histSignal[0].t < cutoff) histSignal.shift();
    while (histSNR.length    > 0 && histSNR[0].t    < cutoff) histSNR.shift();
  }

  // ---------------------------------------------------------------------------
  // Open the modal for a given station
  // ---------------------------------------------------------------------------
  async function open(label) {
    if (_open) close();
    _label = label;
    _open  = true;

    // Clear history and zoom state
    histSignal.length = 0;
    histSNR.length    = 0;
    smoothMag         = null;
    fftView           = null; // reset to full passband view
    dbFloorSmooth     = null; // reset scale smoothing on open
    dbCeilSmooth      = null;
    _lastReading      = null;

    // Show modal
    const modal = document.getElementById('audio-modal');
    modal.classList.remove('hidden');
    document.getElementById('audio-modal-title').textContent     = `🎧 ${label} — Audio Analysis`;
    document.getElementById('audio-modal-subtitle').textContent  = 'Fetching stream info…';
    setStatus('connecting', '⬤ Connecting…');
    document.getElementById('audio-fft-info').textContent        = '';
    document.getElementById('fft-carrier-label').textContent     = '—';

    fftCanvas  = document.getElementById('audio-fft-canvas');
    histCanvas = document.getElementById('audio-history-canvas');

    // Attach zoom/pan interaction to FFT canvas (idempotent — checks flag)
    attachFFTInteraction(fftCanvas);

    // Fetch audio info (sample rate, dial freq)
    try {
      const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
      const r    = await fetch(`${BASE}/api/audio/info?station=${encodeURIComponent(label)}`);
      if (!r.ok) throw new Error(await r.text());
      const info     = await r.json();
      _sampleRate    = info.sample_rate     || 12000;
      _dialFreqHz    = info.dial_freq_hz    || 0;
      _carrierFreqHz = info.carrier_freq_hz || 0;

      document.getElementById('audio-modal-subtitle').textContent =
        `${fmtHzLocal(_carrierFreqHz)} carrier · ${_sampleRate} Hz sample rate · dial ${fmtHzLocal(_dialFreqHz)} (USB)`;
      document.getElementById('fft-carrier-label').textContent = fmtHzLocal(_carrierFreqHz);

      const binHz = _sampleRate / FFT_SIZE;
      document.getElementById('audio-fft-info').textContent =
        `FFT: ${FFT_SIZE} pts · ${binHz.toFixed(3)} Hz/bin · showing ${PASSBAND_LOW}–${PASSBAND_HIGH} Hz passband`;
    } catch (e) {
      setStatus('error', `⬤ Error: ${e.message}`);
      return;
    }

    // Create AudioContext (lazy — must be after a user gesture)
    try {
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: _sampleRate });
    } catch (e) {
      // Fall back to default sample rate if the requested one is unsupported
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }

    // AnalyserNode — Web Audio API does the FFT for us
    _analyser                  = _audioCtx.createAnalyser();
    _analyser.fftSize          = FFT_SIZE;
    _analyser.smoothingTimeConstant = 0; // we do our own smoothing
    _analyser.minDecibels      = -140;
    _analyser.maxDecibels      = 0;
    _analyser.connect(_audioCtx.destination);

    // Single <audio> element — one HTTP connection for both playback and FFT
    const BASE    = (window.BASE_PATH || '').replace(/\/$/, '');
    const audioUrl = `${BASE}/api/audio/preview?station=${encodeURIComponent(label)}`;
    _audioEl       = new Audio(audioUrl);
    _audioEl.crossOrigin = 'anonymous'; // required for Web Audio API tap
    _audioEl.preload     = 'none';

    // Connect audio element → analyser → speakers
    _sourceNode = _audioCtx.createMediaElementSource(_audioEl);
    _sourceNode.connect(_analyser);

    _audioEl.oncanplay = () => {
      setStatus('live', '⬤ Live');
    };
    _audioEl.onerror = () => {
      if (_open) setStatus('error', '⬤ Stream error');
    };
    _audioEl.onended = () => {
      if (_open) close();
    };

    _audioEl.play().catch(e => {
      console.warn('audio play() failed:', e);
      setStatus('error', `⬤ Playback blocked: ${e.message}`);
    });

    // Resume AudioContext if it was suspended (autoplay policy)
    if (_audioCtx.state === 'suspended') {
      _audioCtx.resume().catch(() => {});
    }

    // Start render loop
    _animFrame = requestAnimationFrame(renderLoop);
  }

  // ---------------------------------------------------------------------------
  // Close the modal — stops playback and tears down Web Audio graph
  // ---------------------------------------------------------------------------
  function close() {
    _open  = false;
    _label = null;

    // Stop audio element
    if (_audioEl) {
      _audioEl.oncanplay = null;
      _audioEl.onerror   = null;
      _audioEl.onended   = null;
      _audioEl.pause();
      _audioEl.src = '';
      _audioEl.load(); // abort the HTTP stream
      _audioEl = null;
    }

    // Disconnect Web Audio graph
    if (_sourceNode) {
      try { _sourceNode.disconnect(); } catch (_) {}
      _sourceNode = null;
    }
    if (_analyser) {
      try { _analyser.disconnect(); } catch (_) {}
      _analyser = null;
    }
    if (_audioCtx) {
      _audioCtx.close().catch(() => {});
      _audioCtx = null;
    }

    // Cancel render loop
    if (_animFrame) { cancelAnimationFrame(_animFrame); _animFrame = null; }

    // Hide modal
    const modal = document.getElementById('audio-modal');
    if (modal) modal.classList.add('hidden');

    // Sync app.js state so the Listen button reverts to its default label
    if (typeof state !== 'undefined') {
      state.audioPlaying = null;
      if (typeof renderStatusTable === 'function') renderStatusTable();
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
  // FFT canvas zoom/pan interaction
  // Wheel: zoom towards cursor. Drag: pan. Double-click: reset.
  // Prevents scroll events from reaching the page behind the modal.
  // ---------------------------------------------------------------------------
  function attachFFTInteraction(canvas) {
    if (canvas._fftInteractionAttached) return;
    canvas._fftInteractionAttached = true;

    // Helper: get current full passband bin range (depends on _sampleRate which
    // may not be set yet when this is called, so compute lazily inside handlers)
    function getFullRange() {
      const binHz  = (_sampleRate || 12000) / FFT_SIZE;
      const half   = FFT_SIZE / 2;
      return {
        lo: Math.max(1, Math.floor(PASSBAND_LOW  / binHz)),
        hi: Math.min(half - 1, Math.ceil(PASSBAND_HIGH / binHz)),
      };
    }

    function getView() {
      if (!fftView) fftView = getFullRange();
      return fftView;
    }

    // Mouse wheel → zoom towards cursor position
    canvas.addEventListener('wheel', e => {
      e.preventDefault(); // stop page scroll
      e.stopPropagation();
      const full   = getFullRange();
      const view   = getView();
      const ML     = 46;
      const plotW  = canvas.clientWidth - ML;
      const rect   = canvas.getBoundingClientRect();
      const frac   = Math.max(0, Math.min(1, (e.clientX - rect.left - ML) / plotW));
      const span   = view.hi - view.lo;
      const curBin = view.lo + frac * span;
      const factor = e.deltaY < 0 ? 0.6 : 1.667; // zoom in / out
      const newSpan = Math.max(5, Math.min(full.hi - full.lo, span * factor));
      let newLo = curBin - frac * newSpan;
      let newHi = newLo + newSpan;
      // Clamp to full range
      if (newLo < full.lo) { newLo = full.lo; newHi = newLo + newSpan; }
      if (newHi > full.hi) { newHi = full.hi; newLo = newHi - newSpan; }
      fftView = { lo: newLo, hi: newHi };
    }, { passive: false });

    // Click-drag → pan
    let dragStart = null;
    let dragViewLo = null;
    canvas.addEventListener('mousedown', e => {
      dragStart  = e.clientX;
      dragViewLo = getView().lo;
    });
    canvas.addEventListener('mousemove', e => {
      if (dragStart === null) return;
      const view  = getView();
      const ML    = 46;
      const plotW = canvas.clientWidth - ML;
      const span  = view.hi - view.lo;
      const binsPerPx = span / plotW;
      const dx    = e.clientX - dragStart;
      const full  = getFullRange();
      let newLo   = dragViewLo - dx * binsPerPx;
      newLo = Math.max(full.lo, Math.min(full.hi - span, newLo));
      fftView = { lo: newLo, hi: newLo + span };
    });
    const endDrag = () => { dragStart = null; dragViewLo = null; };
    canvas.addEventListener('mouseup', endDrag);
    canvas.addEventListener('mouseleave', endDrag);

    // Double-click → reset to full passband
    canvas.addEventListener('dblclick', () => { fftView = null; });
  }

  // ---------------------------------------------------------------------------
  // Draw FFT spectrum canvas
  // ---------------------------------------------------------------------------
  function drawFFT() {
    if (!fftCanvas || !_analyser) return;

    const dpr  = window.devicePixelRatio || 1;
    const cssW = fftCanvas.clientWidth  || 760;
    const cssH = fftCanvas.clientHeight || 160;
    const W    = Math.round(cssW * dpr);
    const H    = Math.round(cssH * dpr);
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

    // Read FFT data from AnalyserNode (float dB values)
    const half = _analyser.frequencyBinCount; // = FFT_SIZE / 2
    const freqData = new Float32Array(half);
    _analyser.getFloatFrequencyData(freqData);

    // Check if we have real data yet (all -Infinity means no audio yet)
    let hasData = false;
    for (let i = 1; i < half; i++) {
      if (isFinite(freqData[i]) && freqData[i] > -140) { hasData = true; break; }
    }
    if (!hasData) {
      ctx.fillStyle = '#8b949e';
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('Buffering audio…', ML + plotW / 2, CH / 2);
      return;
    }

    // Smooth
    if (!smoothMag || smoothMag.length !== half) {
      smoothMag = new Float32Array(freqData);
    } else {
      for (let i = 0; i < half; i++) {
        if (isFinite(freqData[i])) {
          smoothMag[i] = smoothMag[i] * (1 - SMOOTH_ALPHA) + freqData[i] * SMOOTH_ALPHA;
        }
      }
    }

    // Determine visible bin range: use zoom view if set, else full passband.
    // AnalyserNode bin i → audio freq = i * sampleRate / FFT_SIZE
    // Real-world freq = dialFreqHz + audio_freq
    const binHz      = _sampleRate / FFT_SIZE;
    const pbLo       = Math.max(1, Math.floor(PASSBAND_LOW  / binHz));
    const pbHi       = Math.min(half - 1, Math.ceil(PASSBAND_HIGH / binHz));
    const binLow     = fftView ? Math.max(pbLo, Math.round(fftView.lo)) : pbLo;
    const binHigh    = fftView ? Math.min(pbHi, Math.round(fftView.hi)) : pbHi;
    const freqStart  = _dialFreqHz + binLow  * binHz;  // real-world Hz at left edge
    const freqEnd    = _dialFreqHz + binHigh * binHz;  // real-world Hz at right edge
    const freqSpan   = freqEnd - freqStart || 1;
    const numBins    = binHigh - binLow + 1;

    // Helper: bin index → X pixel within [ML, ML+plotW]
    const binToX = i => ML + ((i - binLow) / (numBins - 1)) * plotW;

    // dB range over passband bins:
    // - Median (p50) of all bins → robust noise floor estimate (ignores outliers)
    // - Absolute max → ensures the carrier peak always sets the ceiling
    // - Enforce a minimum 40 dB span so the scale never collapses when SNR is low
    let noiseFloorDB = -100, peakDB = -40;
    {
      const vals = [];
      for (let i = binLow; i <= binHigh; i++) {
        if (isFinite(smoothMag[i])) vals.push(smoothMag[i]);
      }
      if (vals.length > 0) {
        vals.sort((a, b) => a - b);
        noiseFloorDB = vals[Math.floor(vals.length * 0.50)]; // median
        peakDB       = vals[vals.length - 1];                // absolute max
      }
    }
    // Enforce minimum span
    if (peakDB - noiseFloorDB < 40) peakDB = noiseFloorDB + 40;

    // Bootstrap smoothed bounds on first valid frame
    if (dbFloorSmooth === null) { dbFloorSmooth = noiseFloorDB; dbCeilSmooth = peakDB; }

    // Noise floor: slow tracking in both directions (stable baseline)
    dbFloorSmooth = dbFloorSmooth * (1 - SCALE_SHRINK_ALPHA) + noiseFloorDB * SCALE_SHRINK_ALPHA;
    // Peak ceiling: snap up fast when signal appears, decay slowly when it fades
    if (peakDB > dbCeilSmooth) {
      dbCeilSmooth = dbCeilSmooth * (1 - SCALE_EXPAND_ALPHA) + peakDB * SCALE_EXPAND_ALPHA;
    } else {
      dbCeilSmooth = dbCeilSmooth * (1 - SCALE_SHRINK_ALPHA) + peakDB * SCALE_SHRINK_ALPHA;
    }

    // Axis bounds: noise floor pushed well below the median; minimal headroom above peak.
    const dbFloor = Math.floor(dbFloorSmooth / 10) * 10 - 20;
    const dbCeil  = Math.ceil(dbCeilSmooth   / 10) * 10 + 5;
    const dbRange = dbCeil - dbFloor || 10;
    const dbToY   = db => plotH - ((db - dbFloor) / dbRange) * plotH;

    // Draw spectrum bars (passband only)
    ctx.fillStyle = '#58a6ff88';
    const barW = Math.max(1, plotW / numBins);
    for (let i = binLow; i <= binHigh; i++) {
      const x = binToX(i);
      const y = dbToY(isFinite(smoothMag[i]) ? smoothMag[i] : dbFloor);
      ctx.fillRect(x, y, barW, plotH - y);
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

    // X axis — real-world frequency labels across the passband (in kHz)
    const rawStep   = freqSpan / 6;
    const mag10     = Math.pow(10, Math.floor(Math.log10(Math.abs(rawStep) || 1)));
    const niceSteps = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
    let freqStep    = (niceSteps.find(v => v * mag10 >= rawStep) || 100) * mag10;
    if (freqStep < 1) freqStep = 1;

    // Determine kHz decimal places based on step size:
    // step >= 1000 Hz → 0 dp, step >= 100 Hz → 1 dp, step >= 10 Hz → 2 dp, else 3 dp
    const khzDecimals = freqStep >= 1000 ? 0 : freqStep >= 100 ? 1 : freqStep >= 10 ? 2 : 3;

    ctx.font = '9px sans-serif';
    ctx.textAlign = 'center';
    const firstLabel = Math.ceil(freqStart / freqStep) * freqStep;
    for (let f = firstLabel; f <= freqEnd; f += freqStep) {
      const x = ML + ((f - freqStart) / freqSpan) * plotW;
      if (x < ML || x > CW) continue;
      ctx.fillStyle = '#8b949e';
      ctx.fillText((f / 1000).toFixed(khzDecimals), x, CH - 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, plotH); ctx.stroke();
      ctx.setLineDash([]);
    }
    ctx.textAlign = 'right';
    ctx.fillStyle = '#555';
    ctx.fillText('kHz', CW - 2, CH - 3);

    // Expected carrier (green dashed) — nominal carrier frequency with no Doppler
    if (_showExpected && _carrierFreqHz >= freqStart && _carrierFreqHz <= freqEnd) {
      const cx = ML + ((_carrierFreqHz - freqStart) / freqSpan) * plotW;
      ctx.strokeStyle = '#3fb950';
      ctx.lineWidth = 1.5;
      ctx.setLineDash([4, 3]);
      ctx.beginPath(); ctx.moveTo(cx, 0); ctx.lineTo(cx, plotH); ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = '#3fb950';
      ctx.font = '9px sans-serif';
      ctx.textAlign = cx > CW - 60 ? 'right' : 'left';
      ctx.fillText('expected', cx + (ctx.textAlign === 'left' ? 3 : -3), 10);
    }

    // Actual detected frequency (red solid) — carrier shifted by measured Doppler
    if (_showActual && _lastReading && _lastReading.valid) {
      const dHz = (_lastReading.corrected_doppler_hz !== null && _lastReading.corrected_doppler_hz !== undefined)
        ? _lastReading.corrected_doppler_hz
        : _lastReading.doppler_hz;
      const actualFreqHz = _carrierFreqHz + dHz;
      if (actualFreqHz >= freqStart && actualFreqHz <= freqEnd) {
        const ax = ML + ((actualFreqHz - freqStart) / freqSpan) * plotW;
        ctx.strokeStyle = '#f85149';
        ctx.lineWidth = 1.5;
        ctx.setLineDash([]);
        ctx.beginPath(); ctx.moveTo(ax, 0); ctx.lineTo(ax, plotH); ctx.stroke();
        ctx.fillStyle = '#f85149';
        ctx.font = '9px sans-serif';
        ctx.textAlign = ax > CW - 60 ? 'right' : 'left';
        ctx.fillText('actual', ax + (ctx.textAlign === 'left' ? 3 : -3), 22);
      }
    }

    // Axis border
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.strokeRect(ML, 0, plotW, plotH);
  }

  // ---------------------------------------------------------------------------
  // Draw 60-second history canvas
  // Dual-axis layout: signal (dBFS) on left axis, SNR (dB) on right axis.
  // Both series share the full plot height for maximum readability.
  // ---------------------------------------------------------------------------
  function drawHistory() {
    if (!histCanvas) return;

    const dpr  = window.devicePixelRatio || 1;
    const cssW = histCanvas.clientWidth  || 760;
    const cssH = histCanvas.clientHeight || 130;
    const W    = Math.round(cssW * dpr);
    const H    = Math.round(cssH * dpr);
    if (histCanvas.width !== W || histCanvas.height !== H) {
      histCanvas.width = W; histCanvas.height = H;
    }
    const ctx = histCanvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const CW = cssW, CH = cssH;

    // Left margin for signal axis, right margin for SNR axis, bottom for time
    const ML = 46, MR = 46, MB = 18;
    const plotW = CW - ML - MR;
    const plotH = CH - MB;

    ctx.clearRect(0, 0, CW, CH);
    ctx.fillStyle = '#0d1117';
    ctx.fillRect(0, 0, CW, CH);

    const now    = Date.now();
    const tStart = now - HISTORY_SECS * 1000;
    const tToX   = t => ML + ((t - tStart) / (HISTORY_SECS * 1000)) * plotW;

    if (histSignal.length < 2) {
      ctx.fillStyle = '#8b949e';
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('Waiting for signal data…', ML + plotW / 2, CH / 2);
      drawTimeAxis(ctx, ML, plotW, plotH, CH);
      ctx.strokeStyle = '#30363d';
      ctx.lineWidth = 1;
      ctx.setLineDash([]);
      ctx.strokeRect(ML, 0, plotW, plotH);
      return;
    }

    // Compute ranges — enforce a minimum 10 dB span so the line is never flat
    const MIN_SPAN = 10;
    let sigMin = Infinity, sigMax = -Infinity;
    let snrMin = Infinity, snrMax = -Infinity;
    for (const p of histSignal) { if (p.v < sigMin) sigMin = p.v; if (p.v > sigMax) sigMax = p.v; }
    for (const p of histSNR)    { if (p.v < snrMin) snrMin = p.v; if (p.v > snrMax) snrMax = p.v; }
    if (!isFinite(sigMin)) { sigMin = -120; sigMax = -40; }
    if (!isFinite(snrMin)) { snrMin = 0;    snrMax = 40; }
    if (sigMax - sigMin < MIN_SPAN) { const m = (sigMin + sigMax) / 2; sigMin = m - MIN_SPAN / 2; sigMax = m + MIN_SPAN / 2; }
    if (snrMax - snrMin < MIN_SPAN) { const m = (snrMin + snrMax) / 2; snrMin = Math.max(0, m - MIN_SPAN / 2); snrMax = snrMin + MIN_SPAN; }

    const sigFloor = Math.floor(sigMin / 5) * 5;
    const sigCeil  = Math.ceil(sigMax  / 5) * 5;
    const sigRange = sigCeil - sigFloor || MIN_SPAN;
    const sigToY   = v => plotH - ((v - sigFloor) / sigRange) * plotH;

    const snrFloor = Math.max(0, Math.floor(snrMin / 5) * 5);
    const snrCeil  = Math.ceil(snrMax / 5) * 5;
    const snrRange = snrCeil - snrFloor || MIN_SPAN;
    const snrToY   = v => plotH - ((v - snrFloor) / snrRange) * plotH;

    // ── Left Y axis: signal power (dBFS) — blue ──────────────────────────────
    ctx.font = '9px sans-serif';
    const sigStep = sigRange <= 20 ? 5 : 10;
    for (let v = sigFloor; v <= sigCeil; v += sigStep) {
      const y = sigToY(v);
      if (y < 0 || y > plotH) continue;
      ctx.fillStyle = '#58a6ff';
      ctx.textAlign = 'right';
      ctx.fillText(v, ML - 3, y + 3);
      ctx.strokeStyle = '#1a2332';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 3]);
      ctx.beginPath(); ctx.moveTo(ML, y); ctx.lineTo(ML + plotW, y); ctx.stroke();
      ctx.setLineDash([]);
    }
    // Left axis label
    ctx.save();
    ctx.fillStyle = '#58a6ff';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText('dBFS', ML - 3, 9);
    ctx.restore();

    // ── Right Y axis: SNR (dB) — green ───────────────────────────────────────
    const snrStep = snrRange <= 20 ? 5 : 10;
    for (let v = snrFloor; v <= snrCeil; v += snrStep) {
      const y = snrToY(v);
      if (y < 0 || y > plotH) continue;
      ctx.fillStyle = '#3fb950';
      ctx.font = '9px sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText(v, ML + plotW + 3, y + 3);
      // Only draw grid lines for SNR ticks that don't already have a signal tick nearby
      // (avoid double grid lines cluttering the plot)
    }
    // Right axis label
    ctx.save();
    ctx.fillStyle = '#3fb950';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'left';
    ctx.fillText('SNR dB', ML + plotW + 3, 9);
    ctx.restore();

    // ── Signal power line (blue) ──────────────────────────────────────────────
    ctx.strokeStyle = '#58a6ff';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    let first = true;
    for (const p of histSignal) {
      const x = tToX(p.t);
      const y = sigToY(p.v);
      if (x < ML || x > ML + plotW) continue;
      if (first) { ctx.moveTo(x, y); first = false; } else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // ── SNR line (green) ──────────────────────────────────────────────────────
    ctx.strokeStyle = '#3fb950';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    first = true;
    for (const p of histSNR) {
      const x = tToX(p.t);
      const y = snrToY(p.v);
      if (x < ML || x > ML + plotW) continue;
      if (first) { ctx.moveTo(x, y); first = false; } else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // Time axis
    drawTimeAxis(ctx, ML, plotW, plotH, CH);

    // Axis border
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.strokeRect(ML, 0, plotW, plotH);
  }

  function drawTimeAxis(ctx, ML, plotW, plotH, CH) {
    ctx.font = '9px sans-serif';
    ctx.textAlign = 'center';
    for (let s = 0; s <= HISTORY_SECS; s += 10) {
      const x     = ML + (s / HISTORY_SECS) * plotW;
      const label = s === 0 ? '−60s' : s === HISTORY_SECS ? 'now' : `−${HISTORY_SECS - s}s`;
      ctx.fillStyle = '#8b949e';
      ctx.fillText(label, x, CH - 3);
      ctx.strokeStyle = '#21262d';
      ctx.lineWidth = 1;
      ctx.setLineDash([2, 2]);
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, plotH); ctx.stroke();
      ctx.setLineDash([]);
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------
  function setStatus(type, text) {
    const el = document.getElementById('audio-status-text');
    if (!el) return;
    el.className  = 'audio-status-' + type;
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
  return {
    open, close, pushReading,
    isOpen:          () => _open,
    activeLabel:     () => _label,
    setShowExpected: v  => { _showExpected = v; },
    setShowActual:   v  => { _showActual   = v; },
  };
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

  // FFT marker visibility toggles
  const showExpectedEl = document.getElementById('fft-show-expected');
  const showActualEl   = document.getElementById('fft-show-actual');
  if (showExpectedEl) showExpectedEl.addEventListener('change', e => AudioAnalysisModal.setShowExpected(e.target.checked));
  if (showActualEl)   showActualEl.addEventListener('change',   e => AudioAnalysisModal.setShowActual(e.target.checked));
});
