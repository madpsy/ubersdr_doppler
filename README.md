# ubersdr_doppler

**HamSCI Doppler shift monitor for UberSDR**

Connects to one or more UberSDR audio channels (one per standard time/frequency station), measures the Doppler shift of each carrier using FFT peak detection, logs minute-mean observations to CSV, and serves a live Chart.js web UI.

This addon supports the [HamSCI Doppler Experiment](https://hamsci.org/doppler-instructions) — a citizen-science project that uses amateur radio receivers to measure ionospheric Doppler shifts on standard time/frequency broadcasts (WWV, WWVH, CHU).

---

## Do I need a GPSDO?

**No — but it affects data quality.**

| Setup | What you get |
|---|---|
| Any SDR (RTL-SDR, Airspy, etc.) | Qualitative data — you can see ionospheric events (solar flares, TIDs, sunrise/sunset terminator) as changes in the Doppler curve shape. The absolute zero point is arbitrary due to hardware clock offset. |
| TCXO-stabilised SDR (Airspy HF+, SDRplay) | Better stability. Slow thermal drift is reduced. Still not GPS-locked. |
| GPS-disciplined SDR or external GPSDO | Quantitative data suitable for HamSCI submission. Absolute Doppler values are meaningful. |

**The key insight:** Without a GPSDO, your hardware clock may be off by tens of Hz at HF. This appears as a constant offset in the Doppler reading (e.g. your "zero" is actually +47 Hz). What matters for ionospheric science is the **variation** around that baseline — the shape of the curve — not the absolute value. The addon displays both the raw reading and a 1-hour baseline mean so you can see the ionospheric component clearly.

**Start collecting data now.** The workflow is the same regardless of hardware. Upgrade to a GPSDO later if you want to contribute precision data to HamSCI.

---

## How It Works

Each monitored station (e.g. WWV 10 MHz) is received as a USB audio channel tuned to `carrier - 500 Hz`, so the carrier appears as a 500 Hz audio tone. A 16384-point FFT (~1.37 s window at 12 kHz) finds the peak bin near 500 Hz and converts the offset to a Doppler shift in Hz:

```
doppler_hz = peak_frequency - 500 Hz
```

Readings are taken every second. Valid readings (SNR ≥ min_snr) are averaged into 1-minute means and written to CSV.

---

## Quick Start

### 1. Add to your UberSDR `addons.yaml`

```yaml
proxies:
  - name: "doppler"
    enabled: true
    host: "doppler"
    port: 6096
    strip_prefix: true
    require_admin: false
    allowed_ips:
      - "0.0.0.0/0"
    rate_limit: 100
```

### 2. Copy and edit `docker-compose.yml`

```bash
cp docker-compose.yml ~/ubersdr/doppler/docker-compose.yml
cd ~/ubersdr/doppler
```

Edit `UBERSDR_URL` to point at your UberSDR instance.

### 3. Start

```bash
docker compose pull
docker compose up -d
```

The web UI is available at `http://your-ubersdr-host/addon/doppler/` (via the UberSDR proxy) or directly at `http://localhost:6096/`.

---

## Configuration

Stations are configured via the web UI and persisted to `doppler_data/stations.json`. No restart is required to add/remove stations.

### Station fields

| Field | Description | Default |
|---|---|---|
| `label` | Display name, e.g. `WWV-10` | required |
| `freq_hz` | Nominal carrier frequency in Hz, e.g. `10000000` | required |
| `callsign` | Your amateur radio callsign (included in CSV) | optional |
| `grid` | Your Maidenhead grid locator (included in CSV) | optional |
| `min_snr` | Minimum SNR in dB to accept a reading | `10.0` |
| `max_drift_hz` | ±Hz search range around the carrier | `100.0` |
| `enabled` | Whether this station is actively monitored | `true` |

### Common stations

| Label | Frequency | Location |
|---|---|---|
| WWV-5 | 5,000,000 Hz | Fort Collins, Colorado |
| WWV-10 | 10,000,000 Hz | Fort Collins, Colorado |
| WWV-15 | 15,000,000 Hz | Fort Collins, Colorado |
| WWV-20 | 20,000,000 Hz | Fort Collins, Colorado |
| WWVH-5 | 5,000,000 Hz | Kauai, Hawaii |
| WWVH-10 | 10,000,000 Hz | Kauai, Hawaii |
| WWVH-15 | 15,000,000 Hz | Kauai, Hawaii |
| CHU-3 | 3,330,000 Hz | Ottawa, Canada |
| CHU-7 | 7,850,000 Hz | Ottawa, Canada |
| CHU-14 | 14,670,000 Hz | Ottawa, Canada |

---

## CSV Output

One CSV file per UTC day per station:

```
doppler_data/YYYY/MM/DD/<label>.csv
```

Columns:

```
timestamp_utc, station, freq_hz, doppler_hz, snr_db, signal_dbfs, noise_dbfs, callsign, grid
```

Download via the web UI or directly:

```
GET /api/csv?station=WWV-10&date=2026-05-01
```

---

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/stations` | List all stations with current reading |
| `GET` | `/api/history?station=<label>` | Minute-mean history (up to 24 h) |
| `GET` | `/api/events` | SSE stream of live readings |
| `GET` | `/api/csv?station=<label>&date=YYYY-MM-DD` | Download daily CSV |
| `POST` | `/api/stations/add` | Add a station (JSON body) |
| `POST` | `/api/stations/update` | Update a station (JSON body, `id` required) |
| `POST` | `/api/stations/remove` | Remove a station (`{"label":"..."}`) |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `UBERSDR_URL` | `ws://ubersdr:8080/ws` | UberSDR WebSocket URL |
| `DOPPLER_DATA_DIR` | `/data` | Data directory for `stations.json` and CSV logs |
| `WEB_PORT` | `6096` | Web UI port |

---

## HamSCI Submission

The CSV files produced by this addon are suitable for submission to the HamSCI Doppler experiment. See [hamsci.org/doppler-instructions](https://hamsci.org/doppler-instructions) for submission details.

Each row represents a 1-minute mean of valid (SNR-gated) Doppler measurements. The `callsign` and `grid` columns identify your station in the aggregated dataset.
