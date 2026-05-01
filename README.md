# ubersdr_doppler

**HamSCI Doppler shift monitor — UberSDR addon**

An addon for [UberSDR](https://ubersdr.org) that monitors standard time/frequency broadcast stations (WWV, WWVH, CHU, etc.), measures the ionospheric Doppler shift of each carrier in real time, logs minute-mean observations to CSV, and serves a live Chart.js web UI.

Supports the [HamSCI Doppler Experiment](https://hamsci.org/doppler-instructions) — a citizen-science project that uses amateur radio receivers to measure ionospheric Doppler shifts on standard time/frequency broadcasts.

---

## Requirements

- A running [UberSDR](https://ubersdr.org) instance
- Docker and Docker Compose
- HF reception of at least one standard time/frequency station (WWV, WWVH, CHU, etc.)

---

## Do I need a GPSDO?

**No — but it affects data quality.**

| Frequency reference | What you get |
|---|---|
| None (UberSDR's internal oscillator) | Qualitative data — you can see ionospheric events (solar flares, TIDs, sunrise/sunset terminator) as changes in the Doppler curve shape. The absolute zero point is arbitrary due to hardware clock offset. |
| GPS-disciplined oscillator (GPSDO) connected to the SDR hardware | Quantitative data suitable for HamSCI submission. Absolute Doppler values are accurate to ~0.01 Hz. |
| Local reference signal (e.g. leaky GPSDO on 10 MHz) | Software correction — mark the reference as a "reference station" in the UI and its Doppler is subtracted from all other stations in real time to cancel hardware clock drift. |

**The key insight:** Without a GPSDO, the hardware clock may be off by tens of Hz at HF. This appears as a constant offset in the Doppler reading. What matters for ionospheric science is the **variation** around that baseline — the shape of the curve — not the absolute value. The addon displays both the raw reading and a 1-hour baseline mean so you can see the ionospheric component clearly.

**Start collecting data now.** The workflow is the same regardless of hardware. Upgrade to a GPSDO later if you want to contribute precision data to HamSCI.

---

## How It Works

Each monitored station (e.g. WWV 10 MHz) is received as an AM audio channel tuned to `carrier - 500 Hz`, so the carrier appears as a 500 Hz audio tone in the demodulated output. A 131,072-point FFT (~10.9 s integration window at 12 kHz, 0.092 Hz/bin) finds the peak bin near 500 Hz and converts the offset to a Doppler shift in Hz using parabolic sub-bin interpolation (~0.01 Hz precision):

```
doppler_hz = peak_frequency - 500 Hz
```

Readings are taken every second. Valid readings (SNR ≥ min_snr) are averaged into 1-minute means and written to CSV.

---

## Quick Start

> **Requires [UberSDR](https://ubersdr.org) to be installed and running first.**

### One-line install

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_doppler/main/install.sh | bash
```

This will:
1. Create `~/ubersdr/doppler/` and download `docker-compose.yml` + helper scripts
2. Pull the latest `madpsy/ubersdr_doppler` image
3. Start the service

Then edit `~/ubersdr/doppler/docker-compose.yml` to set your `UBERSDR_URL` and run `./restart.sh`.

---

### Manual setup

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

Stations and settings are configured via the web UI and persisted to `stations.json` and `settings.json` in the data directory. No restart is required to add/remove stations.

### Global settings

| Setting | Description |
|---|---|
| Callsign | Your amateur radio callsign — default for all stations, included in CSV |
| Grid Square | Your Maidenhead grid locator — default for all stations, included in CSV |
| Frequency Reference | `none` (free-running), `gpsdo` (hardware GPS lock), or `reference_station` (software correction) |
| Reference Description | Optional text describing your reference (e.g. "Leo Bodnar GPSDO on 10 MHz") |

### Station fields

| Field | Description | Default |
|---|---|---|
| `label` | Display name, e.g. `WWV-10` | required |
| `freq_hz` | Nominal carrier frequency in Hz, e.g. `10000000` | required |
| `callsign` | Overrides global callsign for this station | optional |
| `grid` | Overrides global grid for this station | optional |
| `min_snr` | Minimum SNR in dB to accept a reading | `10.0` |
| `max_drift_hz` | ±Hz search range around the carrier | `100.0` |
| `enabled` | Whether this station is actively monitored | `true` |
| `is_reference` | Mark as local reference signal — its Doppler is subtracted from all other stations in real time | `false` |

### Common stations

The web UI includes a preset selector for all common stations. Frequencies for reference:

| Label | Frequency | Location |
|---|---|---|
| WWV-2.5 | 2,500,000 Hz | Fort Collins, Colorado |
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

One CSV file per UTC day per station, stored in the data directory:

```
YYYY/MM/DD/<label>.csv
```

Columns:

```
timestamp_utc, station, freq_hz, doppler_hz, corrected_doppler_hz,
snr_db, signal_dbfs, noise_dbfs, callsign, grid, frequency_reference
```

- `doppler_hz` — raw measured Doppler shift
- `corrected_doppler_hz` — reference-corrected Doppler (populated when a reference station is active)
- `frequency_reference` — `none`, `gpsdo`, or `reference_station` (from global settings)

Download via the web UI or directly:

```
GET /api/csv?station=WWV-10&date=2026-05-01
```

---

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/stations` | List all stations with current reading, 1-hour baseline, and reference correction |
| `GET` | `/api/history?station=<label>` | Minute-mean history (up to 24 h) |
| `GET` | `/api/events` | SSE stream of live readings (1 Hz) |
| `GET` | `/api/csv?station=<label>&date=YYYY-MM-DD` | Download daily CSV |
| `GET` | `/api/settings` | Get global settings |
| `POST` | `/api/settings` | Update global settings (JSON body) |
| `POST` | `/api/stations/add` | Add a station (JSON body) |
| `POST` | `/api/stations/update` | Update a station (JSON body, `id` required) |
| `POST` | `/api/stations/remove` | Remove a station (`{"label":"..."}`) |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `UBERSDR_URL` | `ws://ubersdr:8080/ws` | UberSDR WebSocket URL |
| `DOPPLER_DATA_DIR` | `/data` | Data directory for `stations.json`, `settings.json` and CSV logs |
| `WEB_PORT` | `6096` | Web UI port |

---

## HamSCI Submission

The CSV files produced by this addon are suitable for submission to the HamSCI Doppler experiment. See [hamsci.org/doppler-instructions](https://hamsci.org/doppler-instructions) for submission details.

Each row represents a 1-minute mean of valid (SNR-gated) Doppler measurements. The `callsign`, `grid`, and `frequency_reference` columns identify your station and data quality in the aggregated dataset.
