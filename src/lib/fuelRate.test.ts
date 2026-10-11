import { describe, expect, it } from "vitest";
import {
  LITERS_PER_GALLON,
  formatRateDuration,
  pushLevelSample,
  tankRateReport,
  type LevelSample,
} from "./fuelRate";

// Translated from mobile/test/fuel_predict_test.dart.
// 1000 gal ≈ 3785 L: at that capacity 1%/min == 10 gal/min, keeping the
// %/min ↔ gal/min arithmetic legible in the cases below.
const capacity = 1000 * LITERS_PER_GALLON;

function ramp(start: number, from: number, perMin: number, minutes: number, stepSec = 5) {
  const out: LevelSample[] = [];
  for (let s = 0; s <= minutes * 60; s += stepSec) {
    out.push({ t: start + s * 1000, v: from + (perMin * s) / 60 });
  }
  return out;
}

const t0 = new Date(2026, 7, 18, 12).getTime();
const MIN = 60_000;

describe("tankRateReport", () => {
  it("steady fill reports gal/min and time to 95%", () => {
    // 1%/min of 1000 gal = 10 gal/min; from 60% it's 35 minutes to 95%.
    const r = tankRateReport(ramp(t0, 50, 1, 10), 60, capacity);
    expect(r).not.toBeNull();
    expect(r!.filling).toBe(true);
    expect(r!.gpm).toBeCloseTo(10, 0);
    expect(r!.to95Ms).not.toBeNull();
    expect(Math.abs(r!.to95Ms! / MIN - 35)).toBeLessThanOrEqual(1);
  });

  it("rate comes from the last five minutes only", () => {
    // Flat for 20 minutes, then 2%/min (20 gal/min) for 5.
    const flat = ramp(t0, 40, 0, 20);
    const fill = ramp(t0 + 20 * MIN, 40, 2, 5);
    const r = tankRateReport([...flat, ...fill], 50, capacity);
    expect(r).not.toBeNull();
    expect(Math.abs(r!.gpm - 20)).toBeLessThanOrEqual(1);
  });

  it("draining over 1 gal/min reports usage", () => {
    // -0.5%/min of 1000 gal = 5 gal/min burn.
    const r = tankRateReport(ramp(t0, 80, -0.5, 10), 75, capacity);
    expect(r).not.toBeNull();
    expect(r!.filling).toBe(false);
    expect(Math.abs(r!.gpm - 5)).toBeLessThanOrEqual(0.3);
    expect(r!.to95Ms).toBeNull();
  });

  it("under 1 gal/min either way is ignored as noise", () => {
    // ±0.05%/min of 1000 gal = ±0.5 gal/min — inside the deadband.
    for (const perMin of [0.05, -0.05, 0]) {
      expect(tankRateReport(ramp(t0, 50, perMin, 10), 50, capacity)).toBeNull();
    }
  });

  it("no report without enough history or a capacity", () => {
    const good = ramp(t0, 50, 1, 10);
    // Under 2.5 minutes of history → no trend.
    expect(tankRateReport(ramp(t0, 50, 1, 2), 52, capacity)).toBeNull();
    expect(tankRateReport([], 50, capacity)).toBeNull();
    // No capacity → no gallon rate to judge against.
    expect(tankRateReport(good, 60, null)).toBeNull();
  });

  it("filling above 95% still reports the rate, without an ETA", () => {
    const r = tankRateReport(ramp(t0, 90, 1, 10), 96, capacity);
    expect(r).not.toBeNull();
    expect(r!.filling).toBe(true);
    expect(r!.to95Ms).toBeNull();
  });

  it("works at the web's 30 s poll cadence", () => {
    const r = tankRateReport(ramp(t0, 80, -0.5, 6, 30), 77, capacity);
    expect(r).not.toBeNull();
    expect(r!.filling).toBe(false);
    expect(Math.abs(r!.gpm - 5)).toBeLessThanOrEqual(0.3);
  });
});

describe("pushLevelSample", () => {
  it("trims old samples and skips repeated timestamps", () => {
    let s: LevelSample[] = [];
    s = pushLevelSample(s, { t: t0, v: 50 });
    s = pushLevelSample(s, { t: t0, v: 51 });
    expect(s).toHaveLength(1);
    s = pushLevelSample(s, { t: t0 + 11 * MIN, v: 52 });
    expect(s).toEqual([{ t: t0 + 11 * MIN, v: 52 }]);
  });
});

describe("formatRateDuration", () => {
  it("matches the mobile formatter", () => {
    expect(formatRateDuration(45_000)).toBe("45s");
    expect(formatRateDuration(12 * MIN)).toBe("12m");
    expect(formatRateDuration(65 * MIN)).toBe("1h 5m");
  });
});
