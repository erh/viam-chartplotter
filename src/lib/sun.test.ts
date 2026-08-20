import { describe, it, expect } from "vitest";
import { getSunTimes, isNightAt } from "./sun";

// SunCalc's canonical test fixture: 2013-03-05 UTC at 50.5°N, 30.5°E.
const date = new Date(Date.UTC(2013, 2, 5));
const lat = 50.5;
const lng = 30.5;

describe("getSunTimes", () => {
  it("matches SunCalc's reference sunrise/sunset", () => {
    const t = getSunTimes(date, lat, lng);
    expect(t.sunrise?.toUTCString()).toBe("Tue, 05 Mar 2013 04:34:56 GMT");
    expect(t.sunset?.toUTCString()).toBe("Tue, 05 Mar 2013 15:46:57 GMT");
  });

  it("returns nulls for a polar-night day", () => {
    const t = getSunTimes(new Date(Date.UTC(2013, 11, 21)), 80, 0);
    expect(t.sunrise).toBeNull();
    expect(t.sunset).toBeNull();
  });
});

describe("isNightAt", () => {
  // Sunset 15:46:57 UTC on the fixture day.
  it("stays day until 15 minutes after sunset", () => {
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 15, 55)), lat, lng)).toBe(false);
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 16, 3)), lat, lng)).toBe(true);
  });

  // Sunrise 04:34:56 UTC: night ends 15 minutes BEFORE it.
  it("ends 15 minutes before sunrise", () => {
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 4, 10)), lat, lng)).toBe(true);
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 4, 25)), lat, lng)).toBe(false);
  });

  it("midday is day, midnight is night", () => {
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 12, 0)), lat, lng)).toBe(false);
    expect(isNightAt(new Date(Date.UTC(2013, 2, 5, 0, 30)), lat, lng)).toBe(true);
  });

  it("polar night is always night; polar day never is", () => {
    expect(isNightAt(new Date(Date.UTC(2013, 11, 21, 12)), 80, 0)).toBe(true);
    expect(isNightAt(new Date(Date.UTC(2013, 5, 21, 0)), 80, 0)).toBe(false);
  });
});
