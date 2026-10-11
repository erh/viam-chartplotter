// Tank fill/burn rate — a port of mobile/lib/fuel_screen.dart
// (levelRatePerMin + tankRateReport). Keep the two in sync; the test cases
// in fuelRate.test.ts mirror mobile/test/fuel_predict_test.dart.

export const LITERS_PER_GALLON = 3.78541;

export type LevelSample = { t: number; v: number }; // epoch ms, level %

export type TankRateReport = {
  filling: boolean;
  gpm: number; // always positive; `filling` gives the direction
  to95Ms: number | null; // only while filling below 95%, capped at 24 h
};

/// Level rate (%/min) from the delta over the last 5 minutes of samples.
/// Null when there's not enough history to trust a trend (at least half the
/// window).
export function levelRatePerMin(series: LevelSample[]): number | null {
  if (series.length < 2) return null;
  const last = series[series.length - 1];
  const cutoff = last.t - 5 * 60_000;
  const ref = series.find((s) => s.t >= cutoff);
  if (!ref) return null;
  const windowMin = (last.t - ref.t) / 60_000;
  if (windowMin < 2.5) return null; // too little history to extrapolate
  return (last.v - ref.v) / windowMin;
}

/// What the tank's rate line should report, or null for nothing.
///
/// Rates are in gallons/min (via the tank's liter capacity — without a
/// capacity there's no gallon rate, so no report). Changes slower than
/// 1 gal/min in either direction are ignored as noise. Filling faster than
/// that predicts when the level hits 95%; draining faster reports the burn.
export function tankRateReport(
  series: LevelSample[],
  level: number | null | undefined,
  capacityL: number | null | undefined
): TankRateReport | null {
  const rate = levelRatePerMin(series);
  if (rate == null || capacityL == null || !(capacityL > 0)) return null;
  const gpm = ((rate / 100) * capacityL) / LITERS_PER_GALLON;
  if (gpm >= 1) {
    let to95Ms: number | null = null;
    if (level != null && level < 95) {
      const minutes = (95 - level) / rate;
      if (minutes <= 24 * 60) to95Ms = Math.round(minutes * 60) * 1000;
    }
    return { filling: true, gpm, to95Ms };
  }
  if (gpm <= -1) return { filling: false, gpm: -gpm, to95Ms: null };
  return null; // under 1 gal/min either way — noise, not a fill or a burn
}

/// Append a reading to a tank's series, dropping repeats of the same
/// timestamp and anything older than `keepMs` (the rate only looks back
/// 5 minutes; a little extra covers a slow poll).
export function pushLevelSample(
  series: LevelSample[],
  sample: LevelSample,
  keepMs = 10 * 60_000
): LevelSample[] {
  const last = series[series.length - 1];
  if (last && sample.t <= last.t) return series;
  const cutoff = sample.t - keepMs;
  return [...series.filter((s) => s.t >= cutoff), sample];
}

/// Compact duration, as mobile's _dur: "45s", "12m", "1h 5m".
export function formatRateDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}
