/* audio-analysis.js — Live audio analysis modal for ubersdr_doppler
 *
 * Opens when the user clicks "Listen" on a station row.
 * Supports two modes selectable via the Audio / IQ toggle buttons:
 *
 *   Audio mode (default):
 *     • Connects to /api/audio/preview (USB, 300–1500 Hz passband)
 *     • Single FFT spectrum canvas with real-world frequency axis
 *     • Carrier expected/actual marker lines
 *
 *   IQ mode:
 *     • Connects to /api/iq/stream (stereo WAV, I=left Q=right, ±6 kHz)
 *     • Two stacked FFT canvases — I channel on top, Q channel below
 *     • Full ±6 kHz view centred on the carrier frequency
 *
 * In both modes:
 *   • A single <audio> element is connected to the Web Audio API so that
 *     the browser plays the audio and an AnalyserNode provides FFT data
 *   • 60-second rolling history of signal power (dBFS) and SNR (dB)
 *     sourced from the SSE live feed already running in app.js
 */
'use strict';

// ---------------------------------------------------------------------------
// AudioAnalysisModal — self-contained controller
// ---------------------------------------------------------------------------
const AudioAnalysisModal = (() => {
  // ── State ────────────────────────────────────────────────────────────────
  let _label          = null;
  let _dialFreqHz     = 0;
  let _carrierFreqHz  = 0;
  let _sampleRate     = 12000;
  let _open           = false;
  let _lastReading    = null;  // most recent SSE reading for the active station
  let _showExpected   = true;  // show green 'expected' carrier line
  let _showActual     = true;  // show red 'actual' detected frequency line
  let _mode           = 'audio'; // 'audio' | 'iq'

  // Audio passband limits (Hz, audio-relative from dial frequency).
  // Must match the bandwidthLow/bandwidthHigh sent to UberSDR in doppler.go.
  // The carrier tone appears at 1000 Hz in the audio passband (dial = carrier - 1000).
  const PASSBAND_LOW  = 300;   // Hz above dial frequency
  const PASSBAND_HIGH = 1500;  // Hz above dial frequency

  // IQ bandwidth: ±6 kHz centred on the carrier.
  const IQ_BW_HZ = 6000; // half-bandwidth

  // Web Audio API objects
  let _audioCtx     = null;
  let _audioEl      = null;   // <audio> element — single connection for play + FFT
  let _analyser     = null;   // AnalyserNode (mono for audio; stereo splitter for IQ)
  let _analyserI    = null;   // AnalyserNode for I channel (IQ mode)
  let _analyserQ    = null;   // AnalyserNode for Q channel (IQ mode)
  let _sourceNode   = null;   // MediaElementSourceNode
  let _splitter     = null;   // ChannelSplitterNode (IQ mode)

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
  let fftCanvas     = null;   // audio mode FFT
  let iqICanvas     = null;   // IQ mode — I channel
  let iqQCanvas     = null;   // IQ mode — Q channel
  let histCanvas    = null;

  // Smoothed FFT magnitude buffers (dB, from AnalyserNode.getFloatFrequencyData)
  let smoothMag     = null;   // audio mode
  let smoothMagI    = null;   // IQ mode — I channel
  let smoothMagQ    = null;   // IQ mode — Q channel
  const SMOOTH_ALPHA = 0.3;   // blend factor for new frame (higher = more responsive)

  // Smoothed Y-axis scale bounds — updated slowly so the axis doesn't jump every frame.
  let dbFloorSmooth = null;
  let dbCeilSmooth  = null;
  // Per-channel IQ scale smoothing
  let dbFloorSmoothI = null; let dbCeilSmoothI = null;
  let dbFloorSmoothQ = null; let dbCeilSmoothQ = null;
  const SCALE_EXPAND_ALPHA  = 0.15;
  const SCALE_SHRINK_ALPHA  = 0.005;

  // FFT zoom/pan state (in passband-bin space) — audio mode only
  let fftView = null; // { lo: number, hi: number }

  // ---------------------------------------------------------------------------
  // Public: register a new signal/SNR reading from the SSE feed
  // ---------------------------------------------------------------------------
  function pushReading(reading) {
    if (!_open) return;
    if (reading.valid) _lastReading = reading;
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
    _mode  = 'audio'; // always start in audio mode

    // Clear history and zoom state
    histSignal.length = 0;
    histSNR.length    = 0;
    smoothMag         = null;
    smoothMagI        = null;
    smoothMagQ        = null;
    fftView           = null;
    dbFloorSmooth     = null;
    dbCeilSmooth      = null;
    dbFloorSmoothI    = null; dbCeilSmoothI = null;
    dbFloorSmoothQ    = null; dbCeilSmoothQ = null;
    _lastReading      = null;

    // Show modal
    const modal = document.getElementById('audio-modal');
    modal.classList.remove('hidden');
    document.getElementById('audio-modal-title').textContent     = `🎧 ${label} — Audio Analysis`;
    document.getElementById('audio-modal-subtitle').textContent  = 'Fetching stream info…';
    setStatus('connecting', '⬤ Connecting…');
    document.getElementById('audio-fft-info').textContent        = '';
    document.getElementById('fft-carrier-label').textContent     = '—';
    document.getElementById('iq-centre-label').textContent       = '—';

    fftCanvas  = document.getElementById('audio-fft-canvas');
    iqICanvas  = document.getElementById('audio-iq-i-canvas');
    iqQCanvas  = document.getElementById('audio-iq-q-canvas');
    histCanvas = document.getElementById('audio-history-canvas');

    // Attach zoom/pan interaction to audio FFT canvas (idempotent)
    attachFFTInteraction(fftCanvas);

    // Set initial mode UI
    _applyModeUI();

    // Wire up mode toggle buttons (idempotent via flag)
    _attachModeButtons();

    // Start audio stream (default mode)
    await _startAudioStream();
  }

  // ---------------------------------------------------------------------------
  // Start the audio (USB) stream
  // ---------------------------------------------------------------------------
  async function _startAudioStream() {
    _teardownStream();

    try {
      const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
      const r    = await fetch(`${BASE}/api/audio/info?station=${encodeURIComponent(_label)}`);
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

    try {
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: _sampleRate });
    } catch (e) {
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }

    // Update subtitle with actual AudioContext output rate (may differ from requested
    // if the browser snapped to a supported value, e.g. 44100 instead of 12000).
    {
      const actualRate = _audioCtx.sampleRate;
      const rateNote   = actualRate !== _sampleRate
        ? ` · API out: ${actualRate} Hz`
        : '';
      document.getElementById('audio-modal-subtitle').textContent =
        `${fmtHzLocal(_carrierFreqHz)} carrier · stream: ${_sampleRate} Hz · dial ${fmtHzLocal(_dialFreqHz)} (USB)${rateNote}`;
    }

    _analyser                       = _audioCtx.createAnalyser();
    _analyser.fftSize               = FFT_SIZE;
    _analyser.smoothingTimeConstant = 0;
    _analyser.minDecibels           = -140;
    _analyser.maxDecibels           = 0;
    _analyser.connect(_audioCtx.destination);

    const BASE     = (window.BASE_PATH || '').replace(/\/$/, '');
    const audioUrl = `${BASE}/api/audio/preview?station=${encodeURIComponent(_label)}`;
    _audioEl       = new Audio(audioUrl);
    _audioEl.crossOrigin = 'anonymous';
    _audioEl.preload     = 'none';

    _sourceNode = _audioCtx.createMediaElementSource(_audioEl);
    _sourceNode.connect(_analyser);

    _audioEl.oncanplay = () => { if (_open && _mode === 'audio') setStatus('live', '⬤ Live'); };
    _audioEl.onerror   = () => { if (_open) setStatus('error', '⬤ Stream error'); };
    _audioEl.onended   = () => { if (_open) close(); };

    _audioEl.play().catch(e => {
      console.warn('audio play() failed:', e);
      setStatus('error', `⬤ Playback blocked: ${e.message}`);
    });

    if (_audioCtx.state === 'suspended') _audioCtx.resume().catch(() => {});

    if (!_animFrame) _animFrame = requestAnimationFrame(renderLoop);
  }

  // ---------------------------------------------------------------------------
  // Start the IQ stream
  // ---------------------------------------------------------------------------
  async function _startIQStream() {
    _teardownStream();

    try {
      const BASE = (window.BASE_PATH || '').replace(/\/$/, '');
      const r    = await fetch(`${BASE}/api/iq/info?station=${encodeURIComponent(_label)}`);
      if (!r.ok) throw new Error(await r.text());
      const info     = await r.json();
      _sampleRate    = info.sample_rate    || 12000;
      _carrierFreqHz = info.centre_freq_hz || 0;
      _dialFreqHz    = _carrierFreqHz; // IQ is centred on the carrier

      document.getElementById('audio-modal-subtitle').textContent =
        `${fmtHzLocal(_carrierFreqHz)} centre · ${_sampleRate} Hz sample rate · ±${IQ_BW_HZ / 1000} kHz IQ`;
      document.getElementById('iq-centre-label').textContent = fmtHzLocal(_carrierFreqHz);

      const binHz = _sampleRate / FFT_SIZE;
      document.getElementById('audio-fft-info').textContent =
        `FFT: ${FFT_SIZE} pts · ${binHz.toFixed(3)} Hz/bin · ±${IQ_BW_HZ / 1000} kHz IQ window`;
    } catch (e) {
      setStatus('error', `⬤ Error: ${e.message}`);
      return;
    }

    try {
      // IQ stream is stereo (2 channels) — request matching sample rate
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: _sampleRate });
    } catch (e) {
      _audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }

    // Update subtitle with actual AudioContext output rate.
    {
      const actualRate = _audioCtx.sampleRate;
      const rateNote   = actualRate !== _sampleRate
        ? ` · API out: ${actualRate} Hz`
        : '';
      document.getElementById('audio-modal-subtitle').textContent =
        `${fmtHzLocal(_carrierFreqHz)} centre · stream: ${_sampleRate} Hz · ±${IQ_BW_HZ / 1000} kHz IQ${rateNote}`;
    }

    // Stereo splitter: channel 0 = I (left), channel 1 = Q (right)
    _splitter = _audioCtx.createChannelSplitter(2);

    _analyserI                       = _audioCtx.createAnalyser();
    _analyserI.fftSize               = FFT_SIZE;
    _analyserI.smoothingTimeConstant = 0;
    _analyserI.minDecibels           = -140;
    _analyserI.maxDecibels           = 0;

    _analyserQ                       = _audioCtx.createAnalyser();
    _analyserQ.fftSize               = FFT_SIZE;
    _analyserQ.smoothingTimeConstant = 0;
    _analyserQ.minDecibels           = -140;
    _analyserQ.maxDecibels           = 0;

    // Route: source → splitter → analyserI / analyserQ → silentGain → destination
    // The silent gain (volume=0) keeps the Web Audio graph active so the browser
    // actually decodes the stream and feeds data to the AnalyserNodes.
    // We must NOT set muted=true on the <audio> element — muted elements are not
    // decoded by the Web Audio API in most browsers, which prevents FFT data.
    _splitter.connect(_analyserI, 0);
    _splitter.connect(_analyserQ, 1);
    const silentGain = _audioCtx.createGain();
    silentGain.gain.value = 0;
    _analyserI.connect(silentGain);
    _analyserQ.connect(silentGain);
    silentGain.connect(_audioCtx.destination);

    const BASE   = (window.BASE_PATH || '').replace(/\/$/, '');
    const iqUrl  = `${BASE}/api/iq/stream?station=${encodeURIComponent(_label)}`;
    _audioEl     = new Audio(iqUrl);
    _audioEl.crossOrigin = 'anonymous';
    _audioEl.preload     = 'none';
    // Do NOT set muted=true — muted elements bypass Web Audio decoding in most browsers

    _sourceNode = _audioCtx.createMediaElementSource(_audioEl);
    _sourceNode.connect(_splitter);

    _audioEl.oncanplay = () => { if (_open && _mode === 'iq') setStatus('live', '⬤ Live'); };
    _audioEl.onerror   = () => { if (_open) setStatus('error', '⬤ IQ stream error'); };
    _audioEl.onended   = () => { if (_open) close(); };

    _audioEl.play().catch(e => {
      console.warn('iq play() failed:', e);
      setStatus('error', `⬤ IQ stream blocked: ${e.message}`);
    });

    if (_audioCtx.state === 'suspended') _audioCtx.resume().catch(() => {});

    if (!_animFrame) _animFrame = requestAnimationFrame(renderLoop);
  }

  // ---------------------------------------------------------------------------
  // Tear down the current stream (audio element + Web Audio graph) without
  // closing the modal or resetting history.
  // ---------------------------------------------------------------------------
  function _teardownStream() {
    if (_audioEl) {
      _audioEl.oncanplay = null;
      _audioEl.onerror   = null;
      _audioEl.onended   = null;
      _audioEl.pause();
      _audioEl.src = '';
      _audioEl.load();
      _audioEl = null;
    }
    if (_sourceNode)  { try { _sourceNode.disconnect(); }  catch (_) {} _sourceNode  = null; }
    if (_splitter)    { try { _splitter.disconnect(); }    catch (_) {} _splitter    = null; }
    if (_analyser)    { try { _analyser.disconnect(); }    catch (_) {} _analyser    = null; }
    if (_analyserI)   { try { _analyserI.disconnect(); }   catch (_) {} _analyserI   = null; }
    if (_analyserQ)   { try { _analyserQ.disconnect(); }   catch (_) {} _analyserQ   = null; }
    if (_audioCtx)    { _audioCtx.close().catch(() => {}); _audioCtx = null; }
    if (_animFrame)   { cancelAnimationFrame(_animFrame);  _animFrame = null; }
    // Reset smoothing buffers so the new stream starts fresh
    smoothMag  = null;
    smoothMagI = null;
    smoothMagQ = null;
    dbFloorSmooth = null; dbCeilSmooth = null;
    dbFloorSmoothI = null; dbCeilSmoothI = null;
    dbFloorSmoothQ = null; dbCeilSmoothQ = null;
  }

  // ---------------------------------------------------------------------------
  // Switch between audio and IQ modes
  // ---------------------------------------------------------------------------
  async function _switchMode(newMode) {
    if (newMode === _mode) return;
    _mode = newMode;
    _applyModeUI();
    setStatus('connecting', '⬤ Connecting…');
    if (newMode === 'iq') {
      await _startIQStream();
    } else {
      await _startAudioStream();
    }
  }

  // Show/hide the correct FFT panels and update button active state.
  function _applyModeUI() {
    const audioPanel = document.getElementById('audio-fft-panel');
    const iqPanel    = document.getElementById('iq-fft-panel');
    const btnAudio   = document.getElementById('audio-mode-audio');
    const btnIQ      = document.getElementById('audio-mode-iq');
    if (audioPanel) audioPanel.style.display = _mode === 'audio' ? '' : 'none';
    if (iqPanel)    iqPanel.style.display    = _mode === 'iq'    ? '' : 'none';
    if (btnAudio) { btnAudio.classList.toggle('active', _mode === 'audio'); }
    if (btnIQ)    { btnIQ.classList.toggle('active',    _mode === 'iq');    }
  }

  // Wire up mode toggle buttons — idempotent via a flag on the element.
  function _attachModeButtons() {
    const btnAudio = document.getElementById('audio-mode-audio');
    const btnIQ    = document.getElementById('audio-mode-iq');
    if (btnAudio && !btnAudio._modeListenerAttached) {
      btnAudio._modeListenerAttached = true;
      btnAudio.addEventListener('click', () => _switchMode('audio'));
    }
    if (btnIQ && !btnIQ._modeListenerAttached) {
      btnIQ._modeListenerAttached = true;
      btnIQ.addEventListener('click', () => _switchMode('iq'));
    }
  }

  // ---------------------------------------------------------------------------
  // Close the modal — stops playback and tears down everything
  // ---------------------------------------------------------------------------
  function close() {
    _open  = false;
    _label = null;

    _teardownStream();

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
    if (_mode === 'audio') {
      drawFFT();
    } else {
      if (iqICanvas) drawIQFFT(iqICanvas, _analyserI, 'I', '#58a6ff', 'smoothMagI', 'I');
      if (iqQCanvas) drawIQFFT(iqQCanvas, _analyserQ, 'Q', '#f0883e', 'smoothMagQ', 'Q');
    }
    drawHistory();
    _animFrame = requestAnimationFrame(renderLoop);
  }

  // ---------------------------------------------------------------------------
  // FFT canvas zoom/pan interaction (audio mode only)
  // ---------------------------------------------------------------------------
  function attachFFTInteraction(canvas) {
    if (canvas._fftInteractionAttached) return;
    canvas._fftInteractionAttached = true;

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

    canvas.addEventListener('wheel', e => {
      e.preventDefault();
      e.stopPropagation();
      const full   = getFullRange();
      const view   = getView();
      const ML     = 46;
      const plotW  = canvas.clientWidth - ML;
      const rect   = canvas.getBoundingClientRect();
      const frac   = Math.max(0, Math.min(1, (e.clientX - rect.left - ML) / plotW));
      const span   = view.hi - view.lo;
      const curBin = view.lo + frac * span;
      const factor = e.deltaY < 0 ? 0.6 : 1.667;
      const newSpan = Math.max(5, Math.min(full.hi - full.lo, span * factor));
      let newLo = curBin - frac * newSpan;
      let newHi = newLo + newSpan;
      if (newLo < full.lo) { newLo = full.lo; newHi = newLo + newSpan; }
      if (newHi > full.hi) { newHi = full.hi; newLo = newHi - newSpan; }
      fftView = { lo: newLo, hi: newHi };
    }, { passive: false });

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
    canvas.addEventListener('dblclick', () => { fftView = null; });
  }

  // ---------------------------------------------------------------------------
  // Draw audio-mode FFT spectrum canvas
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

    const half = _analyser.frequencyBinCount;
    const freqData = new Float32Array(half);
    _analyser.getFloatFrequencyData(freqData);

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

    if (!smoothMag || smoothMag.length !== half) {
      smoothMag = new Float32Array(freqData);
    } else {
      for (let i = 0; i < half; i++) {
        if (isFinite(freqData[i])) {
          smoothMag[i] = smoothMag[i] * (1 - SMOOTH_ALPHA) + freqData[i] * SMOOTH_ALPHA;
        }
      }
    }

    const binHz      = _sampleRate / FFT_SIZE;
    const pbLo       = Math.max(1, Math.floor(PASSBAND_LOW  / binHz));
    const pbHi       = Math.min(half - 1, Math.ceil(PASSBAND_HIGH / binHz));
    const binLow     = fftView ? Math.max(pbLo, Math.round(fftView.lo)) : pbLo;
    const binHigh    = fftView ? Math.min(pbHi, Math.round(fftView.hi)) : pbHi;
    const freqStart  = _dialFreqHz + binLow  * binHz;
    const freqEnd    = _dialFreqHz + binHigh * binHz;
    const freqSpan   = freqEnd - freqStart || 1;
    const numBins    = binHigh - binLow + 1;

    const binToX = i => ML + ((i - binLow) / (numBins - 1)) * plotW;

    let noiseFloorDB = -100, peakDB = -40;
    {
      const vals = [];
      for (let i = binLow; i <= binHigh; i++) {
        if (isFinite(smoothMag[i])) vals.push(smoothMag[i]);
      }
      if (vals.length > 0) {
        vals.sort((a, b) => a - b);
        noiseFloorDB = vals[Math.floor(vals.length * 0.50)];
        peakDB       = vals[vals.length - 1];
      }
    }
    if (peakDB - noiseFloorDB < 40) peakDB = noiseFloorDB + 40;

    if (dbFloorSmooth === null) { dbFloorSmooth = noiseFloorDB; dbCeilSmooth = peakDB; }
    dbFloorSmooth = dbFloorSmooth * (1 - SCALE_SHRINK_ALPHA) + noiseFloorDB * SCALE_SHRINK_ALPHA;
    if (peakDB > dbCeilSmooth) {
      dbCeilSmooth = dbCeilSmooth * (1 - SCALE_EXPAND_ALPHA) + peakDB * SCALE_EXPAND_ALPHA;
    } else {
      dbCeilSmooth = dbCeilSmooth * (1 - SCALE_SHRINK_ALPHA) + peakDB * SCALE_SHRINK_ALPHA;
    }

    const dbFloor = Math.floor(dbFloorSmooth / 10) * 10 - 20;
    const dbCeil  = Math.ceil(dbCeilSmooth   / 10) * 10 + 5;
    const dbRange = dbCeil - dbFloor || 10;
    const dbToY   = db => plotH - ((db - dbFloor) / dbRange) * plotH;

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

    // X axis
    const rawStep   = freqSpan / 6;
    const mag10     = Math.pow(10, Math.floor(Math.log10(Math.abs(rawStep) || 1)));
    const niceSteps = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
    let freqStep    = (niceSteps.find(v => v * mag10 >= rawStep) || 100) * mag10;
    if (freqStep < 1) freqStep = 1;

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

    // Expected carrier (green dashed)
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

    // Actual detected frequency (red solid)
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
  // Draw one IQ channel FFT canvas (I or Q)
  // analyser  — the AnalyserNode for this channel
  // smoothKey — 'smoothMagI' or 'smoothMagQ' (string key into closure vars)
  // floorKey  — 'I' or 'Q' (used to pick the right smoothed scale vars)
  // barColour — CSS colour string for the spectrum bars
  // ---------------------------------------------------------------------------
  function drawIQFFT(canvas, analyser, channelLabel, barColour, smoothKey, scaleKey) {
    if (!canvas || !analyser) return;

    // Pick the right smoothing buffer and scale vars by channel
    let smoothBuf   = scaleKey === 'I' ? smoothMagI   : smoothMagQ;
    let floorSmooth = scaleKey === 'I' ? dbFloorSmoothI : dbFloorSmoothQ;
    let ceilSmooth  = scaleKey === 'I' ? dbCeilSmoothI  : dbCeilSmoothQ;

    const dpr  = window.devicePixelRatio || 1;
    const cssW = canvas.clientWidth  || 760;
    const cssH = canvas.clientHeight || 140;
    const W    = Math.round(cssW * dpr);
    const H    = Math.round(cssH * dpr);
    if (canvas.width !== W || canvas.height !== H) {
      canvas.width = W; canvas.height = H;
    }
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const CW = cssW, CH = cssH;

    const ML = 46, MB = 20;
    const plotW = CW - ML, plotH = CH - MB;

    ctx.clearRect(0, 0, CW, CH);
    ctx.fillStyle = '#0d1117';
    ctx.fillRect(0, 0, CW, CH);

    const half = analyser.frequencyBinCount; // FFT_SIZE / 2
    const freqData = new Float32Array(half);
    analyser.getFloatFrequencyData(freqData);

    // Check for real data
    let hasData = false;
    for (let i = 1; i < half; i++) {
      if (isFinite(freqData[i]) && freqData[i] > -140) { hasData = true; break; }
    }
    if (!hasData) {
      ctx.fillStyle = '#8b949e';
      ctx.font = '11px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(`Buffering IQ (${channelLabel})…`, ML + plotW / 2, CH / 2);
      return;
    }

    // Smooth
    if (!smoothBuf || smoothBuf.length !== half) {
      smoothBuf = new Float32Array(freqData);
    } else {
      for (let i = 0; i < half; i++) {
        if (isFinite(freqData[i])) {
          smoothBuf[i] = smoothBuf[i] * (1 - SMOOTH_ALPHA) + freqData[i] * SMOOTH_ALPHA;
        }
      }
    }
    // Write back to closure variable
    if (scaleKey === 'I') smoothMagI = smoothBuf; else smoothMagQ = smoothBuf;

    // IQ view: the Web Audio API gives us bins 0…half-1 where bin i = i * sr / FFT_SIZE Hz
    // (audio-relative). For IQ centred on the carrier, audio freq 0 = carrier,
    // and the ±IQ_BW_HZ window maps to bins 0…(IQ_BW_HZ / binHz).
    // We show the full ±IQ_BW_HZ window: bins 0 to bwBin.
    const binHz  = _sampleRate / FFT_SIZE;
    const bwBin  = Math.min(half - 1, Math.round(IQ_BW_HZ / binHz));
    const binLow = 0;
    const binHigh = bwBin;
    const numBins = binHigh - binLow + 1;

    // Real-world frequency axis: bin 0 = carrier, bin bwBin = carrier + IQ_BW_HZ
    // We display as offset from carrier (−IQ_BW_HZ … +IQ_BW_HZ) by mirroring:
    // For a real IQ stream the negative frequencies are in the upper half of the
    // stereo WAV (Web Audio only gives us 0…Nyquist), so we just show 0…+IQ_BW_HZ
    // labelled as offset from carrier.
    const freqStart = _carrierFreqHz;
    const freqEnd   = _carrierFreqHz + IQ_BW_HZ;
    const freqSpan  = IQ_BW_HZ || 1;

    const binToX = i => ML + ((i - binLow) / (numBins - 1)) * plotW;

    // dB range
    let noiseFloorDB = -100, peakDB = -40;
    {
      const vals = [];
      for (let i = binLow; i <= binHigh; i++) {
        if (isFinite(smoothBuf[i])) vals.push(smoothBuf[i]);
      }
      if (vals.length > 0) {
        vals.sort((a, b) => a - b);
        noiseFloorDB = vals[Math.floor(vals.length * 0.50)];
        peakDB       = vals[vals.length - 1];
      }
    }
    if (peakDB - noiseFloorDB < 40) peakDB = noiseFloorDB + 40;

    if (floorSmooth === null) { floorSmooth = noiseFloorDB; ceilSmooth = peakDB; }
    floorSmooth = floorSmooth * (1 - SCALE_SHRINK_ALPHA) + noiseFloorDB * SCALE_SHRINK_ALPHA;
    if (peakDB > ceilSmooth) {
      ceilSmooth = ceilSmooth * (1 - SCALE_EXPAND_ALPHA) + peakDB * SCALE_EXPAND_ALPHA;
    } else {
      ceilSmooth = ceilSmooth * (1 - SCALE_SHRINK_ALPHA) + peakDB * SCALE_SHRINK_ALPHA;
    }
    // Write back scale vars
    if (scaleKey === 'I') { dbFloorSmoothI = floorSmooth; dbCeilSmoothI = ceilSmooth; }
    else                  { dbFloorSmoothQ = floorSmooth; dbCeilSmoothQ = ceilSmooth; }

    const dbFloor = Math.floor(floorSmooth / 10) * 10 - 20;
    const dbCeil  = Math.ceil(ceilSmooth   / 10) * 10 + 5;
    const dbRange = dbCeil - dbFloor || 10;
    const dbToY   = db => plotH - ((db - dbFloor) / dbRange) * plotH;

    // Spectrum bars
    const colour88 = barColour + '88';
    ctx.fillStyle = colour88;
    const barW = Math.max(1, plotW / numBins);
    for (let i = binLow; i <= binHigh; i++) {
      const x = binToX(i);
      const y = dbToY(isFinite(smoothBuf[i]) ? smoothBuf[i] : dbFloor);
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

    // X axis — real-world frequency labels
    const rawStep   = freqSpan / 6;
    const mag10     = Math.pow(10, Math.floor(Math.log10(Math.abs(rawStep) || 1)));
    const niceSteps = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
    let freqStep    = (niceSteps.find(v => v * mag10 >= rawStep) || 100) * mag10;
    if (freqStep < 1) freqStep = 1;
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

    // Carrier marker (green dashed) — at bin 0 = carrier frequency
    {
      const cx = ML; // bin 0 = left edge = carrier
      ctx.strokeStyle = '#3fb950';
      ctx.lineWidth = 1.5;
      ctx.setLineDash([4, 3]);
      ctx.beginPath(); ctx.moveTo(cx, 0); ctx.lineTo(cx, plotH); ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = '#3fb950';
      ctx.font = '9px sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText('carrier', cx + 3, 10);
    }

    // Channel label (top-right)
    ctx.fillStyle = barColour;
    ctx.font = 'bold 10px sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText(channelLabel, CW - 4, 12);

    // Axis border
    ctx.strokeStyle = '#30363d';
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    ctx.strokeRect(ML, 0, plotW, plotH);
  }

  // ---------------------------------------------------------------------------
  // Draw 60-second history canvas
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

    // Left Y axis: signal power (dBFS) — blue
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
    ctx.save();
    ctx.fillStyle = '#58a6ff';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText('dBFS', ML - 3, 9);
    ctx.restore();

    // Right Y axis: SNR (dB) — green
    const snrStep = snrRange <= 20 ? 5 : 10;
    for (let v = snrFloor; v <= snrCeil; v += snrStep) {
      const y = snrToY(v);
      if (y < 0 || y > plotH) continue;
      ctx.fillStyle = '#3fb950';
      ctx.font = '9px sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText(v, ML + plotW + 3, y + 3);
    }
    ctx.save();
    ctx.fillStyle = '#3fb950';
    ctx.font = '8px sans-serif';
    ctx.textAlign = 'left';
    ctx.fillText('SNR dB', ML + plotW + 3, 9);
    ctx.restore();

    // Signal power line (blue)
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

    // SNR line (green)
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

    drawTimeAxis(ctx, ML, plotW, plotH, CH);

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
    el.className   = 'audio-status-' + type;
    el.textContent = text;
  }

  function fmtHzLocal(hz) {
    if (Math.abs(hz) >= 1e6) return (hz / 1e6).toFixed(2) + ' MHz';
    if (Math.abs(hz) >= 1e3) return (hz / 1e3).toFixed(2) + ' kHz';
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
