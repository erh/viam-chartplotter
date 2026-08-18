import { describe, expect, it } from "vitest";
import { getMoonTimes } from "./moon";

describe("getMoonTimes", () => {
  // Reference values from the upstream SunCalc test suite.
  it("returns the correct moon rise/set times", () => {
    const times = getMoonTimes(new Date("2013-03-04T00:00:00Z"), 50.5, 30.5, true);
    expect(times.rise?.toUTCString()).toBe("Mon, 04 Mar 2013 23:54:29 GMT");
    expect(times.set?.toUTCString()).toBe("Mon, 04 Mar 2013 07:47:58 GMT");
    expect(times.alwaysUp).toBe(false);
    expect(times.alwaysDown).toBe(false);
  });

  it("handles days where the moon does not rise", () => {
    // High-arctic winter: moon stays below the horizon around new moon.
    const times = getMoonTimes(new Date("2023-01-21T00:00:00Z"), 80, 0, true);
    expect(times.rise).toBeNull();
    expect(times.set).toBeNull();
    expect(times.alwaysDown).toBe(true);
    expect(times.alwaysUp).toBe(false);
  });

  it("handles days where the moon stays up", () => {
    // High-arctic winter around full moon: moon circles above the horizon.
    const times = getMoonTimes(new Date("2023-01-06T00:00:00Z"), 80, 0, true);
    expect(times.rise).toBeNull();
    expect(times.set).toBeNull();
    expect(times.alwaysUp).toBe(true);
    expect(times.alwaysDown).toBe(false);
  });
});
